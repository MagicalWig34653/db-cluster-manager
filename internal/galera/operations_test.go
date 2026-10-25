// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func operationState(t *testing.T, size int) ClusterState {
	st := testState(t, size, host("h9", 4000, 8))
	st.Cluster.Image = "mariadb:11.4.13@sha256:" + strings.Repeat("a", 64)
	for i := range st.Nodes {
		st.Nodes[i].Health = healthWith(size, nil)
	}
	return st
}

func TestOperationsSequence(t *testing.T) {
	for _, tc := range []struct {
		req        OperationRequest
		size, want int
	}{
		{OperationRequest{Kind: PlanScaleOut, Slot: "n4"}, 3, 4},
		{OperationRequest{Kind: PlanScaleIn, Slot: "n4"}, 4, 3},
		{OperationRequest{Kind: PlanRollingRestart}, 3, 3},
		{OperationRequest{Kind: PlanMinorUpgrade, Image: "mariadb:11.4.14@sha256:" + strings.Repeat("b", 64)}, 3, 3},
	} {
		t.Run(string(tc.req.Kind), func(t *testing.T) {
			st := operationState(t, tc.size)
			p, err := PlanOperation(st, tc.req, replPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if !p.RequiresApproval || p.Steps[0].Preconditions[0].Kind != PreApproved || len(p.Nodes) != tc.want {
				t.Fatalf("bad plan %+v", p)
			}
			stopped := ""
			verified := false
			for i, s := range p.Steps {
				if s.Kind == StepMarkSafe || (s.Spec != nil && slices.Contains(s.Spec.Args, BootstrapFlag)) {
					t.Fatal("recovery in ordinary operation")
				}
				if i > 0 && !slices.ContainsFunc(s.Preconditions, func(c Precondition) bool { return c.Kind == PreStepSucceeded && c.StepID == p.Steps[i-1].ID }) {
					t.Fatal("missing sequential dependency")
				}
				switch s.Kind {
				case StepStopAll:
					if stopped != "" {
						t.Fatal("two nodes stopped without intervening sync")
					}
					stopped = s.Slot
					verified = false
					if !slices.ContainsFunc(s.Preconditions, func(c Precondition) bool { return c.Kind == PreQuorum }) {
						t.Fatal("stop lacks live quorum")
					}
				case StepVerifyIdentity:
					verified = true
				case StepUpdateService:
					if stopped != s.Slot || !verified {
						t.Fatal("update lacks stop/identity check")
					}
				case StepVerifyDonor:
					if stopped != "" && slices.ContainsFunc(s.Preconditions, func(c Precondition) bool { return c.Kind == PreNodeSynced && c.Slot == stopped }) {
						stopped = ""
					}
				case StepRemoveService:
					if s.Slot != stopped {
						t.Fatal("remove before stop")
					}
					stopped = ""
				}
			}
			if stopped != "" {
				t.Fatal("node left stopped")
			}
			for _, n := range p.Nodes {
				if tc.req.Kind == PlanScaleOut && n.Slot == tc.req.Slot {
					if n.NodeUUID == st.Nodes[0].NodeUUID || n.HostID == st.Nodes[0].HostID {
						t.Fatal("new identity or placement reused")
					}
				}
			}
			if tc.req.Kind == PlanScaleIn && !slices.ContainsFunc(p.Steps, func(s Step) bool { return s.Kind == StepRetainVolume }) {
				t.Fatal("volume not retained")
			}
		})
	}
}

func TestOperationsRefuseUnsafeEvidence(t *testing.T) {
	cases := map[string]func(*ClusterState){
		"stale":      func(s *ClusterState) { s.Nodes[0].ObservedAt = now.Add(-time.Minute) },
		"future":     func(s *ClusterState) { s.Nodes[0].ObservedAt = now.Add(time.Minute) },
		"nonprimary": func(s *ClusterState) { s.Nodes[0].Health.ClusterStatus = "non-Primary" },
		"unknown":    func(s *ClusterState) { s.Nodes[0].Health = NodeHealth{} },
		"donor":      func(s *ClusterState) { s.Nodes[0].Health.LocalState = "Donor/Desynced" },
		"split":      func(s *ClusterState) { s.Nodes[0].Health.StateUUID = "other" },
		"size":       func(s *ClusterState) { s.Nodes[0].Health.ClusterSize = 2 },
		"last-size":  func(s *ClusterState) { s.LastKnownSize = 0 },
		"duplicate":  func(s *ClusterState) { s.Nodes[0] = s.Nodes[1] },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			st := operationState(t, 3)
			mut(&st)
			if _, err := PlanOperation(st, OperationRequest{Kind: PlanRollingRestart}, replPolicy()); err == nil {
				t.Fatal("unsafe operation accepted")
			}
		})
	}
	st := operationState(t, 3)
	for _, req := range []OperationRequest{{Kind: PlanScaleIn, Slot: "n3"}, {Kind: PlanScaleOut, Slot: "n1"}, {Kind: PlanScaleIn, Slot: "absent"}, {Kind: "unknown"}} {
		if _, err := PlanOperation(st, req, replPolicy()); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	pol := replPolicy()
	pol.PausedHosts = []string{"h1"}
	if _, err := PlanOperation(st, OperationRequest{Kind: PlanRollingRestart}, pol); err == nil {
		t.Fatal("maintenance ignored")
	}
	st.Hosts = st.Hosts[:3]
	if _, err := PlanOperation(st, OperationRequest{Kind: PlanScaleOut, Slot: "n4"}, replPolicy()); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("capacity: %v", err)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	from := "mariadb:11.4.13@sha256:" + strings.Repeat("a", 64)
	for _, target := range []string{"mariadb:12.0.1", "mariadb:11.8.1", "mariadb:11.4.12", "mariadb:11.4.13", "other:11.4.14", "mariadb:latest"} {
		err := validateUpgrade(from, target+"@sha256:"+strings.Repeat("b", 64))
		if err == nil {
			t.Fatalf("accepted %s", target)
		}
		if strings.Contains(target, "12.0") || strings.Contains(target, "11.8") {
			if !errors.Is(err, ErrMajorUpgrade) {
				t.Fatalf("missing product approval refusal: %v", err)
			}
		}
	}
	if err := validateUpgrade("mariadb:11.4", from); err == nil {
		t.Fatal("ambiguous source accepted")
	}
}
