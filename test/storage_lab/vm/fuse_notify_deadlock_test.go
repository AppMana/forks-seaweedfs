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

//go:embed fuse_notify_deadlock.sh
var notifyLabScript []byte

var notifyLabResult = regexp.MustCompile(`(?m)^RESULT iteration=(\d+) mode=(\w+) notify_blocked_threads=(\d+) weed_unkillable=(\d+) rename_seconds=([\d.]+) hung_task_reports=(\d+)$`)

type notifyLabIteration struct {
	mode           string
	notifyBlocked  int
	unkillable     bool
	renameSeconds  float64
	hungTaskTotals int
}

func parseNotifyLab(output string) ([]notifyLabIteration, error) {
	if !strings.Contains(output, "NOTIFY_LAB_COMPLETE") {
		return nil, fmt.Errorf("guest scenario did not complete")
	}
	var results []notifyLabIteration
	for _, m := range notifyLabResult.FindAllStringSubmatch(output, -1) {
		blocked, _ := strconv.Atoi(m[3])
		seconds, _ := strconv.ParseFloat(m[5], 64)
		hung, _ := strconv.Atoi(m[6])
		results = append(results, notifyLabIteration{mode: m[2], notifyBlocked: blocked, unkillable: m[4] == "1", renameSeconds: seconds, hungTaskTotals: hung})
	}
	return results, nil
}

// A kernel name invalidation for a directory takes that directory's lock
// uninterruptibly. If a local rename or create in the same directory holds the
// lock while it waits for this mount's reply, the notifying thread cannot run
// or be killed until the reply comes, so a slow filer becomes a hung task, and
// a mount that is exiting can never close /dev/fuse to abort the request.
// appmana-008 and appmana-031 panicked on exactly this on 2026-10-08.
//
// The VM is disposable and offline. Hung-task reports are lowered to 10 s and
// never panic; the host's settings are untouched.
func TestFuseReverseInvalidationLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_NOTIFY_LAB_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_NOTIFY_LAB_LIVE=1")
	}
	weedPath, image := os.Getenv("SEAWEEDFS_LINUX_WEED"), os.Getenv("LABCONTAINERS_VM_IMAGE")
	if !filepath.IsAbs(weedPath) || image == "" {
		t.Fatal("SEAWEEDFS_LINUX_WEED (absolute) and LABCONTAINERS_VM_IMAGE are required")
	}
	iterations := 5
	if v := os.Getenv("SEAWEEDFS_NOTIFY_LAB_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("bad SEAWEEDFS_NOTIFY_LAB_ITERATIONS %q", v)
		}
		iterations = n
	}
	weed, err := os.ReadFile(weedPath)
	if err != nil {
		t.Fatal(err)
	}
	results, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "seaweedfs-notify-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", results)
	if err := os.WriteFile(filepath.Join(results, "provenance.txt"), []byte(fmt.Sprintf("weed=%s sha256=%s\nimage=%s\niterations=%d\n", weedPath, sha(weed), image, iterations)), 0600); err != nil {
		t.Fatal(err)
	}

	lifetime := time.Duration(15+8*iterations) * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	defer cancel()
	topology, err := clab.Source(&core.Config{Name: "seaweedfs-notify", Topology: &types.Topology{
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
		Exec:           &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "linux"}, Argv: []string{"sh", "-ec", "cloud-init status --wait >/dev/null; command -v iptables; echo ready"}, TimeoutMillis: 10000},
		TimeoutMillis:  600000,
		RetryMillis:    2000,
		StdoutContains: []byte("ready"),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := node.Put(ctx, "/opt/weed", 0755, weed); err != nil {
		t.Fatal(err)
	}
	if err := node.Put(ctx, "/opt/notify-lab.sh", 0755, notifyLabScript); err != nil {
		t.Fatal(err)
	}

	run := func(mode string) []notifyLabIteration {
		t.Helper()
		r, err := node.ExecWithTimeout(ctx, time.Duration(3+4*iterations)*time.Minute, "sh", "-c", "/opt/notify-lab.sh /opt/weed "+strconv.Itoa(iterations)+" "+mode+"; rc=$?; tail -n 400 /var/log/notify-lab/*.log /var/log/notify-lab/*.txt 2>/dev/null; exit $rc")
		output := fmt.Sprintf("execution error: %v\nexit: %d\n%s\n%s", err, r.GetExitCode(), r.GetStdout(), r.GetStderr())
		if writeErr := os.WriteFile(filepath.Join(results, "guest-"+mode+".log"), []byte(output), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if err != nil || r.GetExitCode() != 0 {
			diagnosticCtx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			d, derr := node.ExecWithTimeout(diagnosticCtx, 50*time.Second, "sh", "-c", "cat /var/log/notify-lab/progress.txt; ps -eo pid,stat,wchan:32,args | grep -E 'weed|mv' ; for p in $(pgrep -x weed); do for s in /proc/$p/task/*/stack; do grep -q fuse_ $s && { echo \"== $s\"; cat $s; }; done; done; dmesg | tail -n 80; tail -n 100 /var/log/notify-lab/*.log")
			_ = os.WriteFile(filepath.Join(results, "guest-"+mode+"-diagnostics.log"), []byte(fmt.Sprintf("error=%v\n%s\n%s", derr, d.GetStdout(), d.GetStderr())), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseNotifyLab(string(r.GetStdout()))
		if err != nil || len(parsed) != iterations {
			t.Fatalf("%s: %v, %d of %d iterations reported; see %s", mode, err, len(parsed), iterations, results)
		}
		return parsed
	}

	for _, mode := range []string{"wait", "kill"} {
		for i, it := range run(mode) {
			t.Logf("%s iteration %d: notify_blocked_threads=%d weed_unkillable=%t rename_seconds=%.1f hung_task_reports=%d", mode, i+1, it.notifyBlocked, it.unkillable, it.renameSeconds, it.hungTaskTotals)
			if it.notifyBlocked != 0 {
				t.Errorf("%s iteration %d: %d mount threads waited in fuse_reverse_inval_entry behind a stalled local request", mode, i+1, it.notifyBlocked)
			}
			if it.unkillable {
				t.Errorf("%s iteration %d: SIGKILL could not end the mount; its notifier held /dev/fuse open", mode, i+1)
			}
			if it.hungTaskTotals != 0 {
				t.Errorf("%s iteration %d: the guest kernel reported %d hung weed tasks", mode, i+1, it.hungTaskTotals)
			}
		}
	}
}
