package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTopologyHasFourIsolatedVMsAndNoProductionNetwork(t *testing.T) {
	topology := string(topologyYAML("/tmp/network", "qualified/image:tag"))
	if got := strings.Count(topology, "kind: generic_vm"); got != 4 {
		t.Fatalf("generic VM count = %d, want 4\n%s", got, topology)
	}
	if got := strings.Count(topology, "network-mode: none"); got != 5 {
		t.Fatalf("isolated node count = %d, want 5\n%s", got, topology)
	}
	for _, name := range append([]string{controller}, volumes...) {
		if !strings.Contains(topology, name+".yaml:/extra-network.yaml:ro") {
			t.Fatalf("%s network config is not read-only bound", name)
		}
	}
}

func TestNetworkConfigsUseOnlyDocumentationSubnet(t *testing.T) {
	dir := t.TempDir()
	if err := writeNetworkConfigs(dir); err != nil {
		t.Fatal(err)
	}
	for name, address := range addresses {
		data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), address+"/24") || !strings.Contains(string(data), "optional: true") {
			t.Fatalf("unexpected %s network config: %s", name, data)
		}
	}
}

func TestWorkloadIsValidPython(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workload.py")
	if err := os.WriteFile(path, []byte(workload), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("python3", "-m", "py_compile", path).CombinedOutput(); err != nil {
		t.Fatalf("embedded workload is invalid Python: %v\n%s", err, output)
	}
}

func TestVolumeID(t *testing.T) {
	if got := volumeID("7,abc123"); got != "7" {
		t.Fatalf("volumeID = %q", got)
	}
}
