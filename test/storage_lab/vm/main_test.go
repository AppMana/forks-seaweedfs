package main

import (
	"encoding/json"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/links"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTopologyHasFourIsolatedVMsAndNoProductionNetwork(t *testing.T) {
	topology := topologyConfig("/tmp/network", "qualified/image:tag").Topology
	if len(topology.Nodes) != 5 || len(topology.Links) != 4 {
		t.Fatalf("unexpected topology: %+v", topology)
	}
	for name, node := range topology.Nodes {
		if node.NetworkMode != "none" || len(node.Ports) != 0 || node.ImagePullPolicy != "Never" {
			t.Fatalf("node %s has implicit access: %+v", name, node)
		}
	}
	for _, name := range append([]string{controller}, volumes...) {
		node := topology.Nodes[name]
		if node.Kind != "generic_vm" || len(node.Binds) != 1 || node.Binds[0] != "/tmp/network/"+name+".json:/extra-network.yaml:ro" {
			t.Fatalf("%s network config is not read-only bound", name)
		}
	}
	for i, name := range append([]string{controller}, volumes...) {
		link := topology.Links[i].Link.(*links.LinkBriefRaw)
		if len(link.Endpoints) != 2 || link.Endpoints[0] != name+":eth1" {
			t.Fatalf("wrong node data interface: %+v", link)
		}
	}
}

func TestRecoveryRejectsUnrelatedNativeImpact(t *testing.T) {
	for _, plan := range []*core.ApplyResult{
		nil, {}, {DryRun: true, DeployedLab: true},
		{DryRun: true, StartedNodes: []string{controller}},
		{DryRun: true, RecreatedNodes: []string{"switch"}},
		{DryRun: true, RestartedNodes: []string{"volume2"}},
		{DryRun: true, DeletedNodes: []string{"volume1"}},
		{DryRun: true, DeletedEndpoints: []string{"switch:eth2"}},
		{DryRun: true, AddedLinks: []string{"controller:eth1 -- switch:eth1"}},
	} {
		if approveRecovery(plan, "volume1") == nil {
			t.Fatalf("accepted unexpected recovery impact: %+v", plan)
		}
	}
	plan := &core.ApplyResult{DryRun: true, RecreatedNodes: []string{"volume1"}, StartedNodes: []string{"volume1"}, AddedLinks: []string{"switch:eth2 -- volume1:eth1"}}
	if err := approveRecovery(plan, "volume1"); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkConfigsUseOnlyDocumentationSubnet(t *testing.T) {
	dir := t.TempDir()
	if err := writeNetworkConfigs(dir); err != nil {
		t.Fatal(err)
	}
	for name, address := range addresses {
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		nic := config["ethernets"].(map[string]any)["topology"].(map[string]any)
		if config["version"] != float64(2) || nic["addresses"].([]any)[0] != address+"/24" || nic["optional"] != true {
			t.Fatalf("unexpected %s network config: %s", name, data)
		}
		for _, forbidden := range []string{"dhcp4", "dhcp6", "gateway4", "gateway6", "routes", "nameservers"} {
			if _, ok := nic[forbidden]; ok {
				t.Fatalf("implicit network option %s: %s", forbidden, data)
			}
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
