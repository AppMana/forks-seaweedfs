package main

import (
	"fmt"
	"strings"

	labv1 "github.com/appmana/labcontainers/api/v1"
	clab "github.com/appmana/labcontainers/pkg/containerlab"
	"github.com/srl-labs/containerlab/core"
	"github.com/srl-labs/containerlab/links"
	"github.com/srl-labs/containerlab/types"
)

// windowsTopology uses the same native objects as the non-Kubernetes storage
// scenario. No management interface, published port, or egress node is added.
func windowsTopology(image string) (*labv1.TopologySource, error) {
	return clab.Source(&core.Config{Name: "seaweedfs-windows", Topology: &types.Topology{
		Nodes: map[string]*types.NodeDefinition{
			"vm":   {Kind: "generic_vm", Image: image, NetworkMode: "none", ImagePullPolicy: "Never"},
			"peer": {Kind: "linux", Image: "alpine:3.20", NetworkMode: "none", ImagePullPolicy: "Never"},
		},
		Links: []*links.LinkDefinition{{Link: &links.LinkBriefRaw{Endpoints: []string{"vm:eth1", "peer:eth1"}}}},
	}})
}

// approveRecovery is this scenario's assertion, not an SDK policy: losing a
// volume server must not silently reboot its replicas, controller, or switch.
func approveRecovery(plan *core.ApplyResult, victim string) error {
	if plan == nil || !plan.DryRun || plan.DeployedLab || len(plan.DeletedNodes) != 0 || len(plan.DeletedEndpoints) != 0 {
		return fmt.Errorf("recovery requires a target-only native plan: %+v", plan)
	}
	for _, names := range [][]string{plan.AddedNodes, plan.StartedNodes, plan.RestartedNodes, plan.RecreatedNodes} {
		for _, name := range names {
			if name != victim {
				return fmt.Errorf("recovery of %s would mutate %s: %+v", victim, name, plan)
			}
		}
	}
	for _, link := range plan.AddedLinks {
		parts := strings.Split(link, " -- ")
		if len(parts) != 2 || (!strings.HasPrefix(parts[0], victim+":") && !strings.HasPrefix(parts[1], victim+":")) {
			return fmt.Errorf("recovery of %s would change unrelated link %q", victim, link)
		}
	}
	return nil
}

func (h *harness) recoverNode(victim string) error {
	plan, err := h.lab.Plan(h.ctx, nil)
	if err != nil {
		return err
	}
	if err := approveRecovery(plan, victim); err != nil {
		return err
	}
	// Preserve the unmodified native impact with the product's run evidence.
	plans, _ := h.manifest["recovery_plans"].([]*core.ApplyResult)
	h.manifest["recovery_plans"] = append(plans, plan)
	return h.lab.Apply(h.ctx, nil, plan, nil)
}
