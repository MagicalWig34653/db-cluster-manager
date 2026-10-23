// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"testing"
)

func maintNodes() []Node {
	var out []Node
	for i, s := range []string{"n1", "n2", "n3"} {
		n := testNode(s)
		n.HostID = []string{"h1", "h2", "h3"}[i]
		out = append(out, n)
	}
	return out
}

func kinds(p Plan) []StepKind {
	var out []StepKind
	for _, s := range p.Steps {
		out = append(out, s.Kind)
	}
	return out
}

func TestMaintenancePlanStopsGracefullyAfterChecks(t *testing.T) {
	p, err := MaintenancePlan(testCluster(), maintNodes(), "h2")
	if err != nil {
		t.Fatal(err)
	}
	want := []StepKind{StepVerifyDonor, StepStopAll, StepAwaitStopped}
	if got := kinds(p); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("steps %v", got)
	}
	gate := p.Steps[0]
	if gate.Slot != "n2" || len(gate.Preconditions) != 2 {
		t.Fatalf("gate %+v", gate)
	}
	for _, pc := range gate.Preconditions {
		if pc.Kind != PreNodeSynced || pc.ClusterSize != 3 || pc.Slot == "n2" {
			t.Fatalf("precondition %+v", pc)
		}
	}
	if p.Steps[1].Spec == nil || p.Steps[1].Spec.Name != "dbcm-shop-n2" || p.RequiresApproval || p.Kind != PlanHostMaintenance {
		t.Fatalf("plan %+v", p)
	}
	if _, err := MaintenancePlan(testCluster(), maintNodes(), "h9"); !errors.Is(err, ErrNoNodesOnHost) {
		t.Fatalf("no nodes: %v", err)
	}
	// Two of three nodes on the host: one would remain, not a majority.
	nodes := maintNodes()
	nodes[2].HostID = "h2"
	if _, err := MaintenancePlan(testCluster(), nodes, "h2"); !errors.Is(err, ErrMaintenanceUnsafe) {
		t.Fatalf("unsafe: %v", err)
	}
}

func TestMaintenanceEndPlanVerifiesIdentityBeforeStart(t *testing.T) {
	p, err := MaintenanceEndPlan(testCluster(), maintNodes(), "h3")
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(p)
	if len(got) != 3 || got[0] != StepVerifyIdentity || got[1] != StepStartNode || got[2] != StepWaitSynced {
		t.Fatalf("steps %v", got)
	}
	if p.Steps[2].ClusterSize != 3 || p.Steps[1].Preconditions[0].StepID != "verify-n3" {
		t.Fatalf("plan %+v", p.Steps)
	}
}
