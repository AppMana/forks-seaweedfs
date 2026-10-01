package multi_master

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// TestMasterRollingBinaryUpgrade reuses the existing process/port/cleanup
// framework. It never connects to an external cluster or replaces its data.
// Enable with explicit old/new binaries and their SHA-256 pins. Production
// currently uses goraft, so that is the implementation qualified here.
func TestMasterRollingBinaryUpgrade(t *testing.T) {
	baseline := os.Getenv("WEED_MASTER_UPGRADE_BASELINE")
	if baseline == "" {
		t.Skip("set WEED_MASTER_UPGRADE_BASELINE and explicit SHA-256 pins")
	}
	candidate := os.Getenv("WEED_BINARY")
	for _, input := range []struct{ path, pin string }{
		{baseline, os.Getenv("WEED_MASTER_UPGRADE_BASELINE_SHA256")},
		{candidate, os.Getenv("WEED_MASTER_UPGRADE_CANDIDATE_SHA256")},
	} {
		data, err := os.ReadFile(input.path)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != input.pin {
			t.Fatalf("binary hash mismatch: %s", input.path)
		}
		version, err := exec.Command(input.path, "version").CombinedOutput()
		if err != nil {
			t.Fatalf("version: %v: %s", err, version)
		}
		t.Logf("pinned binary %s SHA256=%s version=%s", input.path, input.pin, version)
	}
	if os.Getenv("WEED_MASTER_UPGRADE_BASELINE_SHA256") == os.Getenv("WEED_MASTER_UPGRADE_CANDIDATE_SHA256") {
		t.Fatal("identical binaries cannot qualify a rolling upgrade")
	}
	mc := NewMasterCluster(t, false)
	mc.weedBinary = baseline
	check := func(phase string) {
		t.Helper()
		if _, err := waitForCommonLeader(mc, waitTimeout); err != nil {
			mc.DumpLogs()
			t.Fatalf("%s: %v", phase, err)
		}
		for i := range 3 {
			if err := waitForPeerCount(mc, i, 2, waitTimeout); err != nil {
				mc.DumpLogs()
				t.Fatalf("%s: %v", phase, err)
			}
		}
		t.Logf("ROLLING_MASTER_CONVERGED %s", phase)
	}
	for i := range 3 {
		mc.StartNode(i)
	}
	check("all-baseline")
	for _, i := range []int{2, 1, 0} {
		mc.StopNode(i)
		if _, err := waitForCommonLeader(mc, waitTimeout); err != nil {
			t.Fatalf("quorum while node%d stopped: %v", i, err)
		}
		mc.weedBinary = candidate
		mc.StartNode(i) // same data directory and peer identity
		if mc.nodes[i].cmd.Path != candidate {
			t.Fatal("upgrade did not invoke candidate binary")
		}
		check(fmt.Sprintf("upgraded-node%d", i))
	}
	id, err := mc.WaitForTopologyId(waitTimeout)
	if err != nil {
		mc.DumpLogs()
		t.Fatal(err)
	}
	for _, i := range []int{2, 1, 0} {
		mc.StopNode(i)
		mc.StartNode(i)
		check(fmt.Sprintf("candidate-restart%d", i))
		got, err := mc.WaitForTopologyId(waitTimeout)
		if err != nil || got != id {
			t.Fatalf("topology identity changed across restart: %q -> %q (%v)", id, got, err)
		}
	}
}
