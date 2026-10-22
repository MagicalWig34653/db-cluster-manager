// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

var jobDemandTest = galera.ResourceAmount{MilliCPU: 500, MemoryBytes: 256 << 20}

func policy() SourcePolicy { return SourcePolicy{ClusterSize: 3, Job: jobDemandTest} }

func TestSelectSourcePrefersSyncedNonWriter(t *testing.T) {
	nodes := threeNodes()
	nodes[1].Health.FlowControlPaused = 0.05
	got, _, err := SelectSource(nodes, policy())
	if err != nil || got.Slot != "n3" {
		t.Fatalf("got %s %v; want n3 (non-writer, least flow control)", got.Slot, err)
	}
}

func TestSelectSourceNeverUsesDonorOrDesynced(t *testing.T) {
	nodes := threeNodes()
	nodes[1].Health.LocalState, nodes[1].Health.State = "Donor/Desynced", galera.HealthDegraded
	got, rej, err := SelectSource(nodes, policy())
	if err != nil || got.Slot != "n3" {
		t.Fatalf("got %s %v", got.Slot, err)
	}
	found := false
	for _, r := range rej {
		found = found || r.Slot == "n2"
	}
	if !found {
		t.Fatalf("donor not reported as rejected: %v", rej)
	}
	// Both non-writers donors/desynced: the writer is not used by default.
	nodes[2].Health = nodes[1].Health
	if _, _, err := SelectSource(nodes, policy()); !errors.Is(err, ErrNoSource) {
		t.Fatalf("got %v", err)
	}
}

func TestSelectSourceRefusals(t *testing.T) {
	cases := map[string]func([]NodeState){
		"joiner present (SST running)": func(n []NodeState) { n[2].Health.LocalState = "Joiner" },
		"only one synced node": func(n []NodeState) {
			n[1].Health = galera.NodeHealth{State: galera.HealthUnhealthy}
			n[2].Health = galera.NodeHealth{State: galera.HealthUnhealthy}
		},
		"cluster size differs":  func(n []NodeState) { n[1].Health.ClusterSize, n[2].Health.ClusterSize = 2, 2 },
		"flow control":          func(n []NodeState) { n[1].Health.FlowControlPaused, n[2].Health.FlowControlPaused = 0.5, 0.5 },
		"no capacity":           func(n []NodeState) { n[1].HostFree, n[2].HostFree = galera.ResourceAmount{}, galera.ResourceAmount{} },
		"unsynced non-writers":  func(n []NodeState) { n[1].Health.LocalState, n[2].Health.LocalState = "Joined", "Synced" },
		"missing identity info": func(n []NodeState) { n[1].NodeUUID, n[2].Volume = "", "" },
	}
	for name, mut := range cases {
		nodes := threeNodes()
		mut(nodes)
		if got, _, err := SelectSource(nodes, policy()); !errors.Is(err, ErrNoSource) {
			t.Fatalf("%s: got %s %v", name, got.Slot, err)
		}
	}
	if _, _, err := SelectSource(threeNodes(), SourcePolicy{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing job demand: %v", err)
	}
}

func TestSelectSourceWriterOnlyWithOptIn(t *testing.T) {
	nodes := threeNodes()
	nodes[1].HostFree, nodes[2].HostFree = galera.ResourceAmount{}, galera.ResourceAmount{}
	p := policy()
	p.AllowWriter = true
	got, _, err := SelectSource(nodes, p)
	if err != nil || got.Slot != "n1" {
		t.Fatalf("got %s %v", got.Slot, err)
	}
}

func TestCheckSourceAtGate(t *testing.T) {
	nodes := threeNodes()
	if err := CheckSource(nodes, "n2", policy()); err != nil {
		t.Fatal(err)
	}
	// The chosen donor became desynced between planning and the gate.
	nodes[1].Health.LocalState = "Donor/Desynced"
	if err := CheckSource(nodes, "n2", policy()); !errors.Is(err, ErrNoSource) {
		t.Fatalf("got %v", err)
	}
	nodes = threeNodes()
	nodes[1].Writer, nodes[0].Writer = true, false
	if err := CheckSource(nodes, "n2", policy()); !errors.Is(err, ErrNoSource) {
		t.Fatalf("writer change not refused: %v", err)
	}
	if err := CheckSource(threeNodes(), "n9", policy()); !errors.Is(err, ErrNoSource) {
		t.Fatalf("unknown slot: %v", err)
	}
}
