// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func healthWith(size int, mutate func(map[string]string)) NodeHealth {
	v := syncedVars()
	v["wsrep_cluster_size"] = fmt.Sprint(size)
	if mutate != nil {
		mutate(v)
	}
	return HealthFromStatus(v)
}

var lost = NodeHealth{} // unreachable: never observed since the loss

// testState builds a running cluster of size nodes on hosts h1..hN plus
// extra free hosts; all nodes are Synced at the given size.
func testState(t *testing.T, size int, extra ...Host) ClusterState {
	t.Helper()
	hosts := []Host{}
	for i := 1; i <= size; i++ {
		hosts = append(hosts, host(fmt.Sprintf("h%d", i), 4000, 8))
	}
	tp := testTopology(size)
	p, err := PlanCreate(tp, hosts, PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	st := ClusterState{Cluster: p.Cluster, LastKnownSize: size, Hosts: append(hosts, extra...)}
	for _, n := range p.Nodes {
		st.Nodes = append(st.Nodes, NodeState{Node: n, Health: healthWith(size-1, nil), ObservedAt: now.Add(-5 * time.Second)})
	}
	return st
}

func replPolicy() ReplacementPolicy {
	return ReplacementPolicy{Now: now, NewUUID: seqUUIDs2()}
}

func seqUUIDs2() func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("11111111-0000-4000-8000-%012x", n), nil
	}
}

func setHealth(st *ClusterState, slot string, h NodeHealth) {
	for i := range st.Nodes {
		if st.Nodes[i].Slot == slot {
			st.Nodes[i].Health = h
		}
	}
}

func TestPlanReplacementCreatesNewIdentity(t *testing.T) {
	spare := host("spare", 4000, 8)
	spare.Spare = true
	st := testState(t, 3, host("h9", 16000, 64), spare)
	setHealth(&st, "n3", lost)
	old := st.Nodes[2]

	p, err := PlanReplacement(st, "n3", replPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != PlanReplaceNode || p.RequiresApproval {
		t.Errorf("kind=%s approval=%v", p.Kind, p.RequiresApproval)
	}
	var nn Node
	for _, n := range p.Nodes {
		if n.Slot == "n3" {
			nn = n
		}
	}
	if nn.NodeUUID == old.NodeUUID || nn.Volume == old.Volume || nn.NodeUUID == "" {
		t.Fatalf("replacement reused identity: old %+v new %+v", old.Node, nn)
	}
	// Spare hosts rank before ordinary hosts in replacement, even a bigger one.
	if nn.HostID != "spare" {
		t.Errorf("host = %s, want spare", nn.HostID)
	}
	var kinds []string
	for _, s := range p.Steps {
		kinds = append(kinds, string(s.Kind))
	}
	want := []string{"remove-service", "retain-volume", "create-volume", "init-identity", "init-volume-dirs", "verify-donor", "create-service", "wait-synced"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("steps = %v, want %v", kinds, want)
	}
	rm, keep := p.Steps[0], p.Steps[1]
	if rm.NodeUUID != old.NodeUUID || rm.HostID != old.HostID || keep.Volume != old.Volume || keep.HostID != old.HostID {
		t.Errorf("old identity steps = %+v / %+v", rm, keep)
	}
	init := p.Steps[3]
	if init.NodeUUID != nn.NodeUUID || init.Volume != nn.Volume || init.HostID != "spare" {
		t.Errorf("init = %+v", init)
	}
	if dirs := p.Steps[4]; dirs.NodeUUID != nn.NodeUUID || dirs.Volume != nn.Volume || dirs.HostID != "spare" || dirs.Owner == nil {
		t.Errorf("init-volume-dirs = %+v", dirs)
	}
	cs := p.Steps[6]
	if cs.Spec.Bootstrap() || cs.Spec.Labels[LabelNode] != nn.NodeUUID || cs.Spec.Constraints[0] != "node.id==spare" {
		t.Errorf("create spec = %+v", cs.Spec)
	}
	if !slices.Contains(cs.Spec.Args, "--wsrep_sst_donor=dbcm-shop-n1,") {
		t.Errorf("donor hint missing: %v", cs.Spec.Args)
	}
	if !hasPre(cs, Precondition{Kind: PreQuorum, ClusterSize: 2}) || !hasPre(cs, synced("n1", 2)) {
		t.Errorf("create preconditions = %v", cs.Preconditions)
	}
	if !hasPre(p.Steps[0], Precondition{Kind: PreQuorum, ClusterSize: 2}) {
		t.Errorf("remove preconditions = %v", p.Steps[0].Preconditions)
	}
	if p.Steps[7].ClusterSize != 3 {
		t.Errorf("wait size = %d", p.Steps[7].ClusterSize)
	}
	for _, d := range p.Desired {
		if err := d.ValidateSteadyState(); err != nil {
			t.Error(err)
		}
	}
	if !slices.Equal(p.Cluster.Members, []string{"n1", "n2", "n3"}) {
		t.Errorf("members changed: %v", p.Cluster.Members)
	}
}

func TestPlanReplacementNeverBypassesQuorum(t *testing.T) {
	cases := []struct {
		name   string
		size   int
		failed []string
		mutate func(*ClusterState)
		ok     bool
	}{
		{name: "3 nodes, 1 lost", size: 3, failed: []string{"n3"}, ok: true},
		{name: "3 nodes, 2 lost", size: 3, failed: []string{"n2", "n3"}},
		{name: "5 nodes, 2 lost", size: 5, failed: []string{"n4", "n5"}, ok: true},
		{name: "5 nodes, 3 lost", size: 5, failed: []string{"n3", "n4", "n5"}},
		{
			name: "last known size larger than recorded nodes", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) { st.LastKnownSize = 5 },
		},
		{
			name: "last known size unknown", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) { st.LastKnownSize = 0 },
		},
		{
			name: "survivor observation stale", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) { st.Nodes[1].ObservedAt = now.Add(-10 * time.Minute) },
		},
		{
			name: "survivor observation from the future", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) { st.Nodes[1].ObservedAt = now.Add(10 * time.Minute) },
		},
		{
			name: "survivor health unknown", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) { st.Nodes[1].Health = HealthFromStatus(map[string]string{}) },
		},
		{
			name: "survivors non-Primary (minority view)", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) {
				for i := range 2 {
					st.Nodes[i].Health = healthWith(1, func(v map[string]string) { v["wsrep_cluster_status"] = "non-Primary" })
				}
			},
		},
		{
			name: "survivors disagree on cluster state UUID", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) {
				st.Nodes[1].Health = healthWith(2, func(v map[string]string) { v["wsrep_cluster_state_uuid"] = "b9c3ba34-cbc9-11f1-bf6a-3b478656d12d" })
			},
		},
		{
			name: "survivor reports smaller cluster than quorum", size: 5, failed: []string{"n5"},
			mutate: func(st *ClusterState) { st.Nodes[0].Health = healthWith(2, nil) },
		},
		{
			name: "no Synced donor (all Donor/Desynced)", size: 3, failed: []string{"n3"},
			mutate: func(st *ClusterState) {
				for i := range 2 {
					st.Nodes[i].Health = healthWith(2, func(v map[string]string) { v["wsrep_local_state_comment"] = "Donor/Desynced" })
				}
			},
		},
		{
			name: "one survivor degraded but quorum and donor intact", size: 3, failed: []string{"n3"}, ok: true,
			mutate: func(st *ClusterState) {
				st.Nodes[1].Health = healthWith(2, func(v map[string]string) { v["wsrep_flow_control_paused"] = "0.5" })
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testState(t, tc.size, host("h9", 4000, 8))
			for _, f := range tc.failed {
				setHealth(&st, f, lost)
			}
			for i := range st.Nodes {
				if !slices.Contains(tc.failed, st.Nodes[i].Slot) && st.Nodes[i].Health.InPrimary() {
					st.Nodes[i].Health = healthWith(tc.size-len(tc.failed), nil)
				}
			}
			if tc.mutate != nil {
				tc.mutate(&st)
			}
			_, err := PlanReplacement(st, tc.failed[0], replPolicy())
			if tc.ok {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var ge *GuidedRecoveryError
			if !errors.Is(err, ErrRequiresGuidedRecovery) || !errors.As(err, &ge) || len(ge.Reasons) == 0 {
				t.Fatalf("err = %v, want ErrRequiresGuidedRecovery with reasons", err)
			}
		})
	}
}

// Exhaustive check: for every combination of node health in 3- and 5-node
// clusters, a returned plan always has a verified quorum and donor.
func TestPlanReplacementQuorumInvariant(t *testing.T) {
	healths := []func(size int) NodeHealth{
		func(int) NodeHealth { return lost },
		func(int) NodeHealth { return HealthFromStatus(map[string]string{"wsrep_ready": "ON"}) }, // incomplete
		func(n int) NodeHealth { return healthWith(n, nil) },
		func(n int) NodeHealth {
			return healthWith(n, func(v map[string]string) { v["wsrep_cluster_status"] = "non-Primary" })
		},
		func(n int) NodeHealth {
			return healthWith(n, func(v map[string]string) { v["wsrep_local_state_comment"] = "Joined" })
		},
	}
	plans, refused := map[int]int{}, map[int]int{}
	refusals := func(size int) int { return refused[size] }
	for _, size := range []int{3, 5} {
		combos := 1
		for range size {
			combos *= len(healths)
		}
		for c := range combos {
			st := testState(t, size, host("h9", 4000, 8))
			kinds := make([]int, size)
			members := 0
			for i, x := 0, c; i < size; i, x = i+1, x/len(healths) {
				kinds[i] = x % len(healths)
				if kinds[i] >= 2 {
					members++
				}
			}
			for i := range st.Nodes {
				st.Nodes[i].Health = healths[kinds[i]](members)
			}
			for _, n := range st.Nodes {
				if n.Health.InPrimary() {
					continue
				}
				p, err := PlanReplacement(st, n.Slot, replPolicy())
				if err != nil {
					if !errors.Is(err, ErrRequiresGuidedRecovery) {
						t.Fatalf("size %d combo %d slot %s: unexpected error %v", size, c, n.Slot, err)
					}
					refused[size]++
					continue
				}
				inPrimary, donor := 0, false
				for _, o := range st.Nodes {
					if o.Slot != n.Slot && o.Health.InPrimary() {
						inPrimary++
						donor = donor || o.Health.DonorCapable()
					}
				}
				if inPrimary < size/2+1 || !donor {
					t.Fatalf("size %d combo %d: plan %v without quorum (%d) or donor (%v)", size, c, p.Kind, inPrimary, donor)
				}
				plans[size]++
			}
		}
		if plans[size] == 0 || refusals(size) == 0 {
			t.Fatalf("size %d: invariant test is vacuous (plans %d)", size, plans[size])
		}
	}
}

func TestPlanReplacementRefusals(t *testing.T) {
	st := testState(t, 3, host("h9", 4000, 8))
	if _, err := PlanReplacement(st, "n2", replPolicy()); !errors.Is(err, ErrNodeNotFailed) {
		t.Errorf("healthy node: err = %v", err)
	}
	if _, err := PlanReplacement(st, "n7", replPolicy()); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("unknown slot: err = %v", err)
	}
	setHealth(&st, "n3", lost)
	pol := replPolicy()
	pol.PausedHosts = []string{st.Nodes[2].HostID}
	if _, err := PlanReplacement(st, "n3", pol); !errors.Is(err, ErrReplacementPaused) {
		t.Errorf("maintenance host: err = %v", err)
	}
	// No free host: a capacity error, never a quorum shortcut.
	st.Hosts = st.Hosts[:3]
	_, err := PlanReplacement(st, "n3", replPolicy())
	if !errors.Is(err, ErrNoCapacity) || errors.Is(err, ErrRequiresGuidedRecovery) {
		t.Errorf("no capacity: err = %v", err)
	}
	// The failed node's own host is never reused automatically, even if it is
	// back and free, because its old volume needs reconciliation first.
	var pe *PlacementError
	if !errors.As(err, &pe) || !strings.Contains(pe.Rejected[st.Nodes[2].HostID], "failed node") {
		t.Errorf("rejections = %v", pe)
	}
	// Placement policy applies to replacement too.
	st = testState(t, 3, host("h9", 4000, 8))
	setHealth(&st, "n3", lost)
	pol = replPolicy()
	pol.Placement.Excluded = []string{"h9"}
	if _, err := PlanReplacement(st, "n3", pol); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("excluded only host: err = %v", err)
	}
	pol = replPolicy()
	pol.NewUUID = func() (string, error) { return st.Nodes[2].NodeUUID, nil }
	if _, err := PlanReplacement(st, "n3", pol); err == nil {
		t.Error("reused old UUID accepted")
	}
}
