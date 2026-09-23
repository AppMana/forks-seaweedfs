package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
)

// This host Go test stages the existing storage test executable into Windows.
// The assertions under test remain in weed/storage; they execute on NTFS.
func TestWindowsStorageLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_WINDOWS_LIVE=1")
	}
	runWindowsUnitLab(t, os.Getenv("SEAWEEDFS_WINDOWS_STORAGE_TEST"), []string{"TestWriteNeedle2FsyncsInlineWhileStopping", "TestWriteNeedle2FsyncsIndexBeforeAcknowledging", "TestWriteNeedle2RejectsFailedIndexFsync", "TestWriteNeedle2TruncatesWhenInlineFsyncFails", "TestWriteNeedle2DropsIndexOfUnflushedNewNeedle", "TestStoreWriteVolumeNeedleStaysDurableWhileStopping", "TestReconcileRollForwardMarkerOnly", "TestReconcileRollForwardPartialRename", "TestReconcileRollBackNoMarker", "TestApplyCompactSwapMissingTempFilesPreservesLive"})
}

func TestWindowsMountXAttrLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_UNIT_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_WINDOWS_MOUNT_UNIT_LIVE=1")
	}
	runWindowsUnitLab(t, os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_UNIT_TEST"), []string{"TestXAttrOnUnlinkedOpenFile", "TestXAttrOnRemovedOpenDir", "TestSetAttrOnUnlinkedOpenFile", "TestGetAttrOnUnlinkedOpenFile", "TestSetAttrOnRemovedOpenDir", "TestForgetReleasesRemovedOpenDir"})
}

// Share the isolated Windows VM, staging and strict inventory checks with the
// storage suite; the assertions remain in the original application packages.
func runWindowsUnitLab(t *testing.T, executable string, names []string) {
	t.Helper()
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if len(binary) < 2 || string(binary[:2]) != "MZ" {
		t.Fatal("expected Windows test executable")
	}
	img := os.Getenv("LABCONTAINERS_WINDOWS_IMAGE")
	if img == "" {
		t.Fatal("LABCONTAINERS_WINDOWS_IMAGE required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	c, err := client.Launch(ctx, client.Options{LabdPath: os.Getenv("LABCONTAINERS_LABD")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	topology := []byte(fmt.Sprintf("name: ignored\ntopology:\n  nodes:\n    vm:\n      kind: generic_vm\n      image: %q\n      network-mode: none\n    peer:\n      kind: linux\n      image: alpine:3.20\n      network-mode: none\n  links:\n    - endpoints: [vm:eth1, peer:eth1]\n", img))
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: &labv1.TopologySource{Source: &labv1.TopologySource_Yaml{Yaml: topology}}, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}}, 18*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "vm"}, Argv: []string{ps, "-NoProfile", "-Command", "Write-Output 'windows-ready'"}, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("windows-ready")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Windows ready; staging unit tests SHA-256=%s", sha(binary))
	n := lab.Node("vm")
	if err := n.Put(ctx, `C:\storage.test.exe`, 0600, binary); err != nil {
		t.Fatal(err)
	}
	pattern := "^(" + strings.Join(names, "|") + ")$"
	listed, err := n.Exec(ctx, `C:\storage.test.exe`, "-test.list", pattern)
	if err != nil {
		t.Fatal(err)
	}
	listing := "\n" + strings.ReplaceAll(string(listed.GetStdout()), "\r", "") + "\n"
	for _, name := range names {
		if !strings.Contains(listing, "\n"+name+"\n") {
			t.Fatalf("missing required test %s: %s", name, listing)
		}
	}
	result, err := n.Exec(ctx, `C:\storage.test.exe`, "-test.run", pattern, "-test.v", "-test.count=1", "-test.timeout=90s")
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.GetStdout()) + string(result.GetStderr())
	t.Log(output)
	if result.GetExitCode() != 0 || strings.Contains(output, "--- SKIP:") {
		t.Fatalf("Windows unit tests failed/skipped, exit %d", result.GetExitCode())
	}
	for _, name := range names {
		if !strings.Contains(output, "--- PASS: "+name+" ") {
			t.Fatalf("required test did not pass: %s", name)
		}
	}
}
