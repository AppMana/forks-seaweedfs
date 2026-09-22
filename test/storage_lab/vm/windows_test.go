package main

import (
	"context"
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
	binary, err := os.ReadFile(os.Getenv("SEAWEEDFS_WINDOWS_STORAGE_TEST"))
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
	topology, err := windowsTopology(img)
	if err != nil {
		t.Fatal(err)
	}
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: topology, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}}, 18*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "vm"}, Argv: []string{ps, "-NoProfile", "-Command", "Write-Output 'windows-ready'"}, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("windows-ready")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Windows ready; staging existing storage tests")
	n := lab.Node("vm")
	if err := n.Put(ctx, `C:\storage.test.exe`, 0600, binary); err != nil {
		t.Fatal(err)
	}
	names := []string{"TestWriteNeedle2FsyncsInlineWhileStopping", "TestWriteNeedle2FsyncsIndexBeforeAcknowledging", "TestWriteNeedle2RejectsFailedIndexFsync", "TestWriteNeedle2TruncatesWhenInlineFsyncFails", "TestWriteNeedle2DropsIndexOfUnflushedNewNeedle", "TestStoreWriteVolumeNeedleStaysDurableWhileStopping", "TestReconcileRollForwardMarkerOnly", "TestReconcileRollForwardPartialRename", "TestReconcileRollBackNoMarker", "TestApplyCompactSwapMissingTempFilesPreservesLive"}
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
		t.Fatalf("Windows storage tests failed/skipped, exit %d", result.GetExitCode())
	}
	for _, name := range names {
		if !strings.Contains(output, "--- PASS: "+name+" ") {
			t.Fatalf("required test did not pass: %s", name)
		}
	}
}
