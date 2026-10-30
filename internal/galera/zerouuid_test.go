// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"slices"
	"strings"
	"testing"
)

// Recorded in E12 (tests/p0/results/e12-recovery-autofailover-rolling.md):
// after a total power loss every grastate.dat holds the zero state and
// mariadbd --wsrep-recover answers 0000...:N with the local InnoDB seqno.
func zeroNode(slot string, seqno int64) NodeInfo {
	return NodeInfo{Slot: slot, Grastate: zeroGrastate(), Recovered: &Position{UUID: ZeroUUID, Seqno: seqno}}
}

const grastateEmptyUnsafe = `# GALERA saved state
version: 2.1
uuid:    00000000-0000-0000-0000-000000000000
seqno:   -1
safe_to_bootstrap: 0
`

func zeroGrastate() *Grastate {
	g, err := ParseGrastate(grastateEmptyUnsafe)
	if err != nil {
		panic(err)
	}
	return &g
}

func threeNodes() []Node {
	var nodes []Node
	for i, s := range []string{"n1", "n2", "n3"} {
		n := testNode(s)
		n.NodeUUID = "00000000-0000-4000-8000-00000000000" + string(rune('1'+i))
		nodes = append(nodes, n)
	}
	return nodes
}

func TestDiagnoseZeroUUIDOffersRecoveryInsteadOfNoCandidate(t *testing.T) {
	// B3 state of E12: n1 1303, n2 1304, n3 1302.
	d, err := DiagnoseFullOutage([]NodeInfo{zeroNode("n1", 1303), zeroNode("n2", 1304), zeroNode("n3", 1302)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "n2" || d.Kind != PlanZeroUUIDRecovery || !d.AutoEligible || !d.RequiresApproval {
		t.Fatalf("diagnosis = %+v", d)
	}
	if d.Plan.Kind != PlanZeroUUIDRecovery || len(d.Plan.Steps) == 0 {
		t.Fatalf("plan = %+v", d.Plan)
	}
	for _, p := range d.Positions {
		if p.UUID != ZeroUUID || p.Source != SourceWsrepRecover || p.Seqno < 1300 {
			t.Errorf("position %+v", p)
		}
	}
	if len(d.Risks) != 0 {
		t.Errorf("risks = %+v", d.Risks)
	}
	joined := strings.Join(d.Reasons, "\n")
	if !strings.Contains(joined, "new state UUID") {
		t.Errorf("reasons do not explain the new history: %v", d.Reasons)
	}
	ms := stepsOf(d.Plan, StepMarkSafe)
	if len(ms) != 1 || ms[0].Slot != "n2" || !ms[0].ZeroUUID || ms[0].Position == nil || ms[0].Position.UUID != ZeroUUID ||
		ms[0].Position.Seqno != -1 || ms[0].Recovered != nil {
		t.Fatalf("mark-safe = %+v", ms)
	}
}

func TestDiagnoseZeroUUIDAmbiguityNeverQualifiesForAuto(t *testing.T) {
	cases := []struct {
		name  string
		nodes []NodeInfo
		cand  string
		risk  RiskKind
	}{
		{"tie at the top", []NodeInfo{zeroNode("n1", 1304), zeroNode("n2", 1304), zeroNode("n3", 1302)}, "n1", RiskTie},
		{"one position unreadable", []NodeInfo{zeroNode("n1", 1303), zeroNode("n2", 1304),
			{Slot: "n3", Grastate: mustGrastate(t, grastateEmptyUnsafe)}}, "n2", RiskPositionUnknown},
		{"one node unreachable", []NodeInfo{zeroNode("n1", 1303), zeroNode("n2", 1304), {Slot: "n3"}}, "n2", RiskMissingNode},
		{"recovered seqno -1", []NodeInfo{zeroNode("n1", 1303), zeroNode("n2", 1304), zeroNode("n3", -1)}, "n2", RiskPositionUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := DiagnoseFullOutage(c.nodes)
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind != PlanZeroUUIDRecovery || d.Candidate != c.cand || d.AutoEligible || !d.RequiresApproval || !hasRisk(d, c.risk, "") {
				t.Fatalf("diagnosis = %+v", d)
			}
		})
	}
}

func TestDiagnoseZeroUUIDWithoutPositionsOrMajority(t *testing.T) {
	// GET diagnosis without wsrep-recover: nothing readable, no candidate,
	// but the reason says which operation applies.
	d, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Grastate: mustGrastate(t, grastateEmptyUnsafe)}, {Slot: "n2", Grastate: mustGrastate(t, grastateEmptyUnsafe)},
		{Slot: "n3", Grastate: mustGrastate(t, grastateEmptyUnsafe)}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "" || d.AutoEligible || d.Kind != PlanZeroUUIDRecovery || len(d.Plan.Steps) != 0 {
		t.Fatalf("diagnosis = %+v", d)
	}
	if !strings.Contains(strings.Join(d.Reasons, "\n"), "wsrep-recover") {
		t.Errorf("reasons = %v", d.Reasons)
	}
	// Only one of three readable: no majority, no candidate.
	d, _ = DiagnoseFullOutage([]NodeInfo{zeroNode("n1", 5), {Slot: "n2", Grastate: mustGrastate(t, grastateEmptyUnsafe)}, {Slot: "n3"}})
	if d.Candidate != "" || d.AutoEligible {
		t.Fatalf("minority diagnosis = %+v", d)
	}
}

func TestDiagnoseMixedZeroAndRealHistoryIsNotTheZeroUUIDFlow(t *testing.T) {
	// A node with a real cluster UUID next to zero-state nodes is diverged
	// data: the ordinary diagnosis applies (no auto, risks shown).
	d, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "900", "0"))},
		zeroNode("n2", 1304), zeroNode("n3", 1303)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind == PlanZeroUUIDRecovery || d.AutoEligible {
		t.Fatalf("mixed history treated as zero-UUID: %+v", d)
	}
}

func TestZeroUUIDRecoveryPlanOrder(t *testing.T) {
	d, _ := DiagnoseFullOutage([]NodeInfo{zeroNode("n1", 1303), zeroNode("n2", 1304), zeroNode("n3", 1302)})
	nodes := threeNodes()
	p, err := RecoveryPlan(d, testCluster(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	p = p.WithSnapshot("pre-recovery-op-1")
	var seq []string
	for _, s := range p.Steps {
		seq = append(seq, string(s.Kind)+":"+s.Slot)
	}
	want := []string{
		"stop-all:n2", "stop-all:n1", "stop-all:n3",
		"await-stopped:n2", "await-stopped:n1", "await-stopped:n3",
		"verify-identity:n2", "verify-identity:n1", "verify-identity:n3",
		"snapshot-data:n2", "snapshot-data:n1", "snapshot-data:n3",
		"mark-safe-to-bootstrap:n2", "update-service:n2", "start-node:n2", "wait-synced:n2",
		"start-node:n1", "wait-synced:n1", "start-node:n3", "wait-synced:n3",
		"update-service:n2", "await-update:n2", "wait-synced:n2", "check-recovered-state:",
	}
	if !slices.Equal(seq, want) {
		t.Fatalf("steps =\n%v\nwant\n%v", seq, want)
	}
	if !hasPre(p.Steps[0], Precondition{Kind: PreApproved}) {
		t.Error("first step is not approval-gated")
	}
	for _, s := range p.Steps {
		switch s.Kind {
		case StepSnapshotData:
			if s.Snapshot != "pre-recovery-op-1" || s.HostID == "" || s.Volume == "" || !hasPre(s, Precondition{Kind: PreNoNodeRunning}) {
				t.Errorf("snapshot step %+v", s)
			}
		case StepMarkSafe:
			// The bootstrap never precedes the complete set of snapshots.
			for _, o := range stepsOf(p, StepSnapshotData) {
				if idx(p, o.ID) > idx(p, s.ID) {
					t.Errorf("snapshot %s after mark-safe", o.ID)
				}
			}
		case StepCheckState:
			if !hasPre(s, Precondition{Kind: PreStateRecovered, ClusterSize: 3}) {
				t.Errorf("check step %+v", s)
			}
		}
	}
	// Recovery never changes the image: every desired spec keeps it.
	for _, ds := range p.Desired {
		if ds.Image != testCluster().Image {
			t.Errorf("desired image %q", ds.Image)
		}
	}
}

func idx(p Plan, id string) int {
	return slices.IndexFunc(p.Steps, func(s Step) bool { return s.ID == id })
}

func TestPlanZeroUUIDPrepare(t *testing.T) {
	nodes := threeNodes()
	p, err := BuildZeroUUIDPrepare(testCluster(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	p = p.WithSnapshot("pre-recovery-op-9")
	if p.Kind != PlanZeroUUIDPrepare || !p.RequiresApproval || !hasPre(p.Steps[0], Precondition{Kind: PreApproved}) {
		t.Fatalf("plan = %+v", p)
	}
	var seq []string
	for _, s := range p.Steps {
		seq = append(seq, string(s.Kind)+":"+s.Slot)
		if s.Kind == StepStopAll || s.Kind == StepAwaitStopped {
			if s.Spec == nil || s.Spec.Name == "" || s.HostID == "" {
				t.Errorf("step %s lacks service or host", s.ID)
			}
		}
		if s.Kind == StepSnapshotData && s.Snapshot != "pre-recovery-op-9" {
			t.Errorf("snapshot name %q", s.Snapshot)
		}
	}
	want := []string{"stop-all:n1", "stop-all:n2", "stop-all:n3", "await-stopped:n1", "await-stopped:n2", "await-stopped:n3",
		"verify-identity:n1", "verify-identity:n2", "verify-identity:n3", "snapshot-data:n1", "snapshot-data:n2", "snapshot-data:n3"}
	if !slices.Equal(seq, want) {
		t.Fatalf("steps =\n%v\nwant\n%v", seq, want)
	}
	// Nothing in the prepare phase writes to the data or starts a node.
	for _, s := range p.Steps {
		if s.Kind == StepMarkSafe || s.Kind == StepStartNode || s.Kind == StepUpdateService {
			t.Errorf("prepare plan contains %s", s.Kind)
		}
	}
	if _, err := BuildZeroUUIDPrepare(testCluster(), nodes[:2]); err == nil {
		t.Error("two nodes of three accepted")
	}
}
