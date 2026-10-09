package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	clab "github.com/appmana/labcontainers/pkg/containerlab"
	"github.com/appmana/labcontainers/pkg/client"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/types"
)

//go:embed fuse_stuck_backend.sh
var stuckLabScript []byte

const (
	stuckLabRequestTimeout = 4 // seconds, below the guest's 10 s hung-task timeout
	stuckLabKernelTimeout  = 7
	// 32 MiB cap at 16 MiB/s, plus the metadata commit and the last chunks.
	stuckLabCloseBound = 6.0
)

var (
	stuckLabOp    = regexp.MustCompile(`(?m)^RESULT phase=stuck op=(\w+) seconds=([\d.]+) returned=(\d)`)
	stuckLabClose = regexp.MustCompile(`(?m)^RESULT phase=close write_seconds=([\d.]+) close_seconds=([\d.]+)$`)
	stuckLabHung  = regexp.MustCompile(`(?m)^RESULT phase=end hung_task_reports=(\d+)$`)
)

type stuckLabReport struct {
	ops           map[string]float64 // seconds; absent if the op never returned
	unreturned    []string
	writeSeconds  float64
	closeSeconds  float64
	intact        bool
	hungTaskTotal int
}

func parseStuckLab(output string) (stuckLabReport, error) {
	r := stuckLabReport{ops: map[string]float64{}}
	if !strings.Contains(output, "STUCK_LAB_COMPLETE") {
		return r, fmt.Errorf("guest scenario did not complete")
	}
	for _, m := range stuckLabOp.FindAllStringSubmatch(output, -1) {
		if m[3] != "1" {
			r.unreturned = append(r.unreturned, m[1])
			continue
		}
		r.ops[m[1]], _ = strconv.ParseFloat(m[2], 64)
	}
	m := stuckLabClose.FindStringSubmatch(output)
	if m == nil {
		return r, fmt.Errorf("no close-phase result")
	}
	r.writeSeconds, _ = strconv.ParseFloat(m[1], 64)
	r.closeSeconds, _ = strconv.ParseFloat(m[2], 64)
	r.intact = strings.Contains(output, "RESULT phase=close intact=1")
	h := stuckLabHung.FindStringSubmatch(output)
	if h == nil {
		return r, fmt.Errorf("no hung-task count")
	}
	r.hungTaskTotal, _ = strconv.Atoi(h[1])
	return r, nil
}

// A mount whose backend stops answering must never leave a task in
// uninterruptible sleep past the hung-task timeout: on the cluster's nodes
// hung_task_panic=1 turns that into a node panic (appmana-031, 2026-10-09,
// a SIGKILLed writer's FLUSH on a mount whose cache directory was deleted).
// The guest runs with the same panic setting, so a regression loses the VM.
// A large sequential write must also close within the write buffer cap's
// drain time instead of draining an unbounded backlog at close().
func TestFuseStuckBackendLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_STUCK_LAB_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_STUCK_LAB_LIVE=1")
	}
	weedPath, image := os.Getenv("SEAWEEDFS_LINUX_WEED"), os.Getenv("LABCONTAINERS_VM_IMAGE")
	if !filepath.IsAbs(weedPath) || image == "" {
		t.Fatal("SEAWEEDFS_LINUX_WEED (absolute) and LABCONTAINERS_VM_IMAGE are required")
	}
	// SEAWEEDFS_STUCK_LAB_REPORT_ONLY=1 reports hung tasks instead of
	// panicking the guest, to see which operation hangs when the gate fails.
	hungTaskPanic := 1
	if os.Getenv("SEAWEEDFS_STUCK_LAB_REPORT_ONLY") == "1" {
		hungTaskPanic = 0
	}
	weed, err := os.ReadFile(weedPath)
	if err != nil {
		t.Fatal(err)
	}
	results, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "seaweedfs-stuck-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", results)
	if err := os.WriteFile(filepath.Join(results, "provenance.txt"), []byte(fmt.Sprintf("weed=%s sha256=%s\nimage=%s\nhung_task_panic=%d\n", weedPath, sha(weed), image, hungTaskPanic)), 0600); err != nil {
		t.Fatal(err)
	}

	lifetime := 30 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	defer cancel()
	topology, err := clab.Source(&core.Config{Name: "seaweedfs-stuck", Topology: &types.Topology{
		Nodes: map[string]*types.NodeDefinition{
			"linux": {Kind: "generic_vm", Image: image, NetworkMode: "none", ImagePullPolicy: "Never"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Launch(ctx, client.Options{LabdPath: os.Getenv("LABCONTAINERS_LABD")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: topology, Nodes: map[string]*labv1.NodeExtension{"linux": {Control: "qga"}}, ArtifactDirectory: results}, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	node := lab.Node("linux")
	if _, err := lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{
		Exec:           &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "linux"}, Argv: []string{"sh", "-ec", "cloud-init status --wait >/dev/null; command -v tc; command -v python3; echo ready"}, TimeoutMillis: 10000},
		TimeoutMillis:  600000,
		RetryMillis:    2000,
		StdoutContains: []byte("ready"),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := node.Put(ctx, "/opt/weed", 0755, weed); err != nil {
		t.Fatal(err)
	}
	if err := node.Put(ctx, "/opt/stuck-lab.sh", 0755, stuckLabScript); err != nil {
		t.Fatal(err)
	}

	r, err := node.ExecWithTimeout(ctx, 15*time.Minute, "sh", "-c", fmt.Sprintf("/opt/stuck-lab.sh /opt/weed %d %d %d; rc=$?; dmesg | grep -A12 'blocked for more than'; tail -n 200 /var/log/stuck-lab/*.log /var/log/stuck-lab/*.txt 2>/dev/null; exit $rc", stuckLabRequestTimeout, stuckLabKernelTimeout, hungTaskPanic))
	output := fmt.Sprintf("execution error: %v\nexit: %d\n%s\n%s", err, r.GetExitCode(), r.GetStdout(), r.GetStderr())
	if writeErr := os.WriteFile(filepath.Join(results, "guest.log"), []byte(output), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || r.GetExitCode() != 0 || !strings.Contains(string(r.GetStdout()), "STUCK_LAB_COMPLETE") {
		diagnosticCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		d, derr := node.ExecWithTimeout(diagnosticCtx, 50*time.Second, "sh", "-c", "uptime; cat /var/log/stuck-lab/progress.txt; ps -eo pid,stat,wchan:32,args | grep -E 'weed|qemu-ga|head|dd|stat|mkdir|python' ; dmesg | tail -n 120; journalctl -u qemu-guest-agent --no-pager | tail -n 40; tail -n 100 /var/log/stuck-lab/*.log")
		_ = os.WriteFile(filepath.Join(results, "guest-diagnostics.log"), []byte(fmt.Sprintf("error=%v\n%s\n%s", derr, d.GetStdout(), d.GetStderr())), 0600)
	}
	if err != nil {
		// A hung-task panic halts the guest, so the agent stops answering.
		t.Fatalf("guest did not finish the scenario (a hung-task panic halts it): %v; see %s", err, results)
	}
	report, err := parseStuckLab(string(r.GetStdout()))
	if err != nil {
		t.Fatalf("%v; see %s", err, results)
	}
	bound := float64(2*stuckLabRequestTimeout + 1)
	for _, op := range []string{"lookup", "mkdir", "read", "write_close", "fsync", "killed_writer_exit"} {
		seconds, ok := report.ops[op]
		switch {
		case !ok:
			t.Errorf("%s did not return while the backend was frozen", op)
		case seconds > bound:
			t.Errorf("%s returned after %.1f s, bound %.0f s", op, seconds, bound)
		default:
			t.Logf("%s returned after %.1f s", op, seconds)
		}
	}
	t.Logf("256 MiB sequential write took %.1f s, close() %.1f s", report.writeSeconds, report.closeSeconds)
	if report.closeSeconds > stuckLabCloseBound {
		t.Errorf("close() after a large write took %.1f s, bound %.1f s", report.closeSeconds, stuckLabCloseBound)
	}
	if !report.intact {
		t.Error("large file did not read back intact")
	}
	if report.hungTaskTotal != 0 {
		t.Errorf("the guest kernel reported %d hung tasks", report.hungTaskTotal)
	}
}
