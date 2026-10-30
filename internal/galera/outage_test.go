// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Recorded in E1 (tests/p0/results/e1-swarm-galera.log) after `docker kill`:
// unclean shutdown, seqno -1.
const grastateE1Crash = `# GALERA saved state
version: 2.1
uuid:    9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e
seqno:   -1
safe_to_bootstrap: 0
`

// Clean shutdown of the last node to leave, in the same file format.
const grastateClean = `# GALERA saved state
version: 2.1
uuid:    9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e
seqno:   1234
safe_to_bootstrap: 1
`

// Fresh datadir that never completed a state transfer. E2 logged
// "Found saved state: 00000000-0000-0000-0000-000000000000:-1,
// safe_to_bootstrap: 1" for such a node.
const grastateEmpty = `# GALERA saved state
version: 2.1
uuid:    00000000-0000-0000-0000-000000000000
seqno:   -1
safe_to_bootstrap: 1
`

// mariadbd --wsrep-recover log tail. The line format follows the MariaDB
// documentation; it was NOT recorded in the lab (E1/E2 did not run
// --wsrep-recover).
const wsrepRecoverOut = `2026-10-19 14:40:01 0 [Note] Starting MariaDB 11.4.13-MariaDB-ubu2404 source revision as process 1
2026-10-19 14:40:01 0 [Note] InnoDB: Starting crash recovery from checkpoint LSN=47566
2026-10-19 14:40:02 0 [Note] InnoDB: 128 rollback segments are active.
2026-10-19 14:40:02 0 [Note] WSREP: Recovered position: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e:1240
`

func TestParseGrastate(t *testing.T) {
	g, err := ParseGrastate(grastateE1Crash)
	if err != nil {
		t.Fatal(err)
	}
	if g.Version != "2.1" || g.UUID != "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e" || g.Seqno != -1 || g.SafeToBootstrap || !g.HasSafeToBootstrap {
		t.Errorf("crash = %+v", g)
	}
	g, err = ParseGrastate(grastateClean)
	if err != nil || g.Seqno != 1234 || !g.SafeToBootstrap {
		t.Errorf("clean = %+v, %v", g, err)
	}
	g, err = ParseGrastate(grastateEmpty)
	if err != nil || !g.Empty() {
		t.Errorf("empty = %+v, %v", g, err)
	}
	// Older format without safe_to_bootstrap, CRLF line endings.
	g, err = ParseGrastate("# GALERA saved state\r\nversion: 2.0\r\nuuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\r\nseqno: 7\r\n")
	if err != nil || g.Seqno != 7 || g.HasSafeToBootstrap {
		t.Errorf("v2.0 = %+v, %v", g, err)
	}
	for name, bad := range map[string]string{
		"empty file":     "",
		"no uuid":        "version: 2.1\nseqno: 5\n",
		"no seqno":       "uuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\n",
		"bad uuid":       "uuid: xyz\nseqno: 5\n",
		"bad seqno":      "uuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\nseqno: five\n",
		"seqno below -1": "uuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\nseqno: -2\n",
		"bad safe flag":  "uuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\nseqno: 5\nsafe_to_bootstrap: yes\n",
		"duplicate key":  "uuid: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e\nseqno: 5\nseqno: 6\n",
	} {
		if _, err := ParseGrastate(bad); !errors.Is(err, ErrBadRecoveryInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestParseWsrepRecover(t *testing.T) {
	p, err := ParseWsrepRecover(wsrepRecoverOut)
	if err != nil || p.UUID != "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e" || p.Seqno != 1240 {
		t.Fatalf("pos = %+v, %v", p, err)
	}
	// GTID suffix and multiple lines: the last one wins.
	p, err = ParseWsrepRecover("x WSREP: Recovered position: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e:3\n" +
		"y [Note] WSREP: Recovered position: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e:9,0-1-9\n")
	if err != nil || p.Seqno != 9 {
		t.Fatalf("pos = %+v, %v", p, err)
	}
	for name, bad := range map[string]string{
		"no line":   "2026-10-19 [Note] InnoDB: done\n",
		"bad uuid":  "WSREP: Recovered position: abc:5\n",
		"bad seqno": "WSREP: Recovered position: 9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e:x\n",
	} {
		if _, err := ParseWsrepRecover(bad); !errors.Is(err, ErrBadRecoveryInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func mustGrastate(t *testing.T, s string) *Grastate {
	t.Helper()
	g, err := ParseGrastate(s)
	if err != nil {
		t.Fatal(err)
	}
	return &g
}

func mustRecover(t *testing.T, s string) *Position {
	t.Helper()
	p, err := ParseWsrepRecover(s)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

func withSeqno(s string, seqno string, safe string) string {
	s = strings.Replace(s, "seqno:   1234", "seqno:   "+seqno, 1)
	return strings.Replace(s, "safe_to_bootstrap: 1", "safe_to_bootstrap: "+safe, 1)
}

func hasRisk(d Diagnosis, k RiskKind, slot string) bool {
	return slices.ContainsFunc(d.Risks, func(r Risk) bool { return r.Kind == k && (slot == "" || r.Slot == slot) })
}

func TestDiagnoseCleanShutdownPicksSafeNode(t *testing.T) {
	d, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "1230", "0"))},
		{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "1234", "1"))},
		{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "1232", "0"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "n2" || !d.RequiresApproval || !d.Plan.RequiresApproval {
		t.Fatalf("diagnosis = %+v", d)
	}
	if len(d.Risks) != 0 {
		t.Errorf("risks = %+v", d.Risks)
	}
	if len(d.Reasons) == 0 {
		t.Error("no reasons")
	}
	// n2 already has safe_to_bootstrap: 1; the mark-safe step still re-checks
	// that its grastate.dat holds the diagnosed position before the bootstrap.
	ms := stepsOf(d.Plan, StepMarkSafe)
	if len(ms) != 1 || ms[0].Slot != "n2" || ms[0].Position == nil || *ms[0].Position != (Position{UUID: d.Positions[1].UUID, Seqno: 1234}) {
		t.Errorf("mark-safe steps = %+v", ms)
	}
	assertRecoveryPlan(t, d.Plan, "n2", []string{"n1", "n3"})
}

func TestDiagnoseCrashUsesRecoveredPositions(t *testing.T) {
	d, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Grastate: mustGrastate(t, grastateE1Crash), Recovered: mustRecover(t, strings.Replace(wsrepRecoverOut, ":1240", ":1238", 1))},
		{Slot: "n2", Grastate: mustGrastate(t, grastateE1Crash), Recovered: mustRecover(t, wsrepRecoverOut)},
		{Slot: "n3", Grastate: mustGrastate(t, grastateE1Crash), Recovered: mustRecover(t, strings.Replace(wsrepRecoverOut, ":1240", ":1239", 1))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "n2" {
		t.Fatalf("candidate = %q (%+v)", d.Candidate, d)
	}
	for _, s := range []string{"n1", "n2", "n3"} {
		if !hasRisk(d, RiskUncleanShutdown, s) {
			t.Errorf("no unclean-shutdown risk for %s", s)
		}
	}
	ms := stepsOf(d.Plan, StepMarkSafe)
	if len(ms) != 1 || ms[0].Slot != "n2" {
		t.Errorf("mark-safe steps = %+v", ms)
	}
	// The re-check binds to the file as read (seqno -1 after the crash), not
	// to the recovered seqno.
	if p := ms[0].Position; p == nil || p.Seqno != -1 || p.UUID != d.Positions[1].UUID {
		t.Errorf("mark-safe position = %+v", ms[0].Position)
	}
	// ... and writes the recovered seqno, so the bootstrap keeps the history.
	if r := ms[0].Recovered; r == nil || r.Seqno != 1240 || r.UUID != d.Positions[1].UUID {
		t.Errorf("mark-safe recovered = %+v", ms[0].Recovered)
	}
	pos := d.Positions[1]
	if pos.Source != SourceWsrepRecover || pos.Seqno != 1240 {
		t.Errorf("n2 position = %+v", pos)
	}
}

func TestDiagnoseRisks(t *testing.T) {
	t.Run("crash without wsrep-recover: positions unknown, no candidate", func(t *testing.T) {
		d, err := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, grastateE1Crash)},
			{Slot: "n2", Grastate: mustGrastate(t, grastateE1Crash)},
			{Slot: "n3", Grastate: mustGrastate(t, grastateE1Crash)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if d.Candidate != "" || len(d.Plan.Steps) != 0 || !d.RequiresApproval {
			t.Fatalf("diagnosis = %+v", d)
		}
		if !hasRisk(d, RiskPositionUnknown, "n1") {
			t.Errorf("risks = %+v", d.Risks)
		}
	})
	t.Run("one node unknown: candidate proposed with risk", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "1234", "0"))},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "1233", "0"))},
			{Slot: "n3"},
		})
		if d.Candidate != "n1" || !hasRisk(d, RiskMissingNode, "n3") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("majority unknown: no candidate", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, grastateClean)},
			{Slot: "n2"},
			{Slot: "n3", Grastate: mustGrastate(t, grastateE1Crash)},
		})
		if d.Candidate != "" || !hasRisk(d, RiskMissingNode, "n2") || !hasRisk(d, RiskPositionUnknown, "n3") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("diverging cluster UUIDs: no candidate", func(t *testing.T) {
		other := strings.Replace(grastateClean, "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e", "b9c3ba34-cbc9-11f1-bf6a-3b478656d12d", 1)
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, grastateClean)},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "1200", "0"))},
			{Slot: "n3", Grastate: mustGrastate(t, other)},
		})
		if d.Candidate != "" || !hasRisk(d, RiskDivergingUUIDs, "") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("empty node with safe_to_bootstrap 1 is never a candidate", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "900", "0"))},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "901", "0"))},
			{Slot: "n3", Grastate: mustGrastate(t, grastateEmpty)},
		})
		if d.Candidate != "n2" || !hasRisk(d, RiskNoData, "n3") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("wsrep-recover without state is never a candidate", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Recovered: &Position{UUID: ZeroUUID, Seqno: 5000}},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "10", "0"))},
			{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "9", "0"))},
		})
		if d.Candidate != "n2" || !hasRisk(d, RiskNoData, "n1") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("safe flag on a node behind the highest seqno", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "1000", "1"))},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "1005", "0"))},
			{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "1001", "0"))},
		})
		if d.Candidate != "n2" || !hasRisk(d, RiskSafeFlagMismatch, "n1") {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
	t.Run("tie prefers safe_to_bootstrap, then slot order", func(t *testing.T) {
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "50", "0"))},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "50", "1"))},
			{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "49", "0"))},
		})
		if d.Candidate != "n2" {
			t.Errorf("candidate = %q", d.Candidate)
		}
		d, _ = DiagnoseFullOutage([]NodeInfo{
			{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "50", "0"))},
			{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "50", "0"))},
			{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "49", "0"))},
		})
		if d.Candidate != "n1" || !hasRisk(d, RiskTie, "") {
			t.Errorf("candidate = %q risks = %+v", d.Candidate, d.Risks)
		}
	})
	t.Run("recovered UUID differs from grastate UUID", func(t *testing.T) {
		rec := strings.Replace(wsrepRecoverOut, "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e", "b9c3ba34-cbc9-11f1-bf6a-3b478656d12d", 1)
		d, _ := DiagnoseFullOutage([]NodeInfo{
			{Slot: "n1", Grastate: mustGrastate(t, grastateE1Crash), Recovered: mustRecover(t, rec)},
			{Slot: "n2", Grastate: mustGrastate(t, grastateClean)},
			{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "1200", "0"))},
		})
		if !hasRisk(d, RiskRecoveredUUIDMismatch, "n1") || d.Candidate != "n2" {
			t.Fatalf("diagnosis = %+v", d)
		}
	})
}

func TestDiagnoseRefusesWhenPrimaryExists(t *testing.T) {
	_, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Health: healthWith(2, nil)},
		{Slot: "n2", Health: healthWith(2, nil)},
		{Slot: "n3", Grastate: mustGrastate(t, grastateE1Crash)},
	})
	if !errors.Is(err, ErrNotFullOutage) {
		t.Fatalf("err = %v", err)
	}
	if _, err := DiagnoseFullOutage(nil); !errors.Is(err, ErrBadRecoveryInput) {
		t.Errorf("empty input: err = %v", err)
	}
	if _, err := DiagnoseFullOutage([]NodeInfo{{Slot: "n1"}, {Slot: "n1"}}); !errors.Is(err, ErrBadRecoveryInput) {
		t.Errorf("duplicate slot: err = %v", err)
	}
}

func TestDiagnoseRefusesZeroUUIDPrimary(t *testing.T) {
	v := syncedVars()
	v["wsrep_cluster_state_uuid"] = ZeroUUID
	_, err := DiagnoseFullOutage([]NodeInfo{{Slot: "n1", Health: HealthFromStatus(v)}, {Slot: "n2"}, {Slot: "n3"}})
	if !errors.Is(err, ErrZeroUUIDPrimary) {
		t.Fatalf("zero-UUID Primary must block recovery: %v", err)
	}
}

func TestRecoveryPlanAttachesSpecs(t *testing.T) {
	d, err := DiagnoseFullOutage([]NodeInfo{
		{Slot: "n1", Grastate: mustGrastate(t, withSeqno(grastateClean, "10", "0"))},
		{Slot: "n2", Grastate: mustGrastate(t, withSeqno(grastateClean, "12", "0"))},
		{Slot: "n3", Grastate: mustGrastate(t, withSeqno(grastateClean, "11", "0"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []Node
	for i, s := range []string{"n1", "n2", "n3"} {
		n := testNode(s)
		n.NodeUUID = []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"}[i]
		nodes = append(nodes, n)
	}
	p, err := RecoveryPlan(d, testCluster(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequiresApproval || p.Kind != PlanFullOutageRecovery {
		t.Fatalf("plan = %+v", p)
	}
	for _, s := range p.Steps {
		if s.Kind != StepUpdateService {
			continue
		}
		if s.Spec == nil {
			t.Fatalf("step %s without spec", s.ID)
		}
		boot := s.Slot == "n2" && strings.Contains(s.Note, "bootstrap spec")
		if s.Spec.Bootstrap() != boot {
			t.Errorf("step %s (%s) bootstrap=%v", s.ID, s.Note, s.Spec.Bootstrap())
		}
		// The bootstrap spec is applied to the stopped service; update-service
		// never changes the replica count.
		if want := map[bool]uint64{true: 0, false: 1}[boot]; s.Spec.Replicas != want {
			t.Errorf("step %s replicas = %d, want %d", s.ID, s.Spec.Replicas, want)
		}
	}
	for _, s := range p.Steps {
		switch s.Kind {
		case StepStopAll, StepAwaitStopped, StepStartNode, StepAwaitUpdate, StepVerifyIdentity, StepMarkSafe:
			if s.HostID == "" || s.NodeUUID == "" || s.Volume == "" {
				t.Errorf("step %s lacks host, node or volume: %+v", s.ID, s)
			}
			if s.Kind != StepVerifyIdentity && s.Kind != StepMarkSafe && (s.Spec == nil || s.Spec.Name == "") {
				t.Errorf("step %s lacks the service name", s.ID)
			}
		}
	}
	for _, ds := range p.Desired {
		if err := ds.ValidateSteadyState(); err != nil {
			t.Error(err)
		}
	}
	if _, err := RecoveryPlan(Diagnosis{RequiresApproval: true}, testCluster(), nodes); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("no candidate: err = %v", err)
	}
	if _, err := RecoveryPlan(d, testCluster(), nodes[:2]); err == nil {
		t.Error("missing node accepted")
	}
}

// assertRecoveryPlan checks the guided recovery step order: approval, stop
// all, verify identities, snapshot, bootstrap candidate, then others one at a time,
// then remove the bootstrap flag and check the recovered state.
func assertRecoveryPlan(t *testing.T, p Plan, cand string, others []string) {
	t.Helper()
	if p.Kind != PlanFullOutageRecovery || !p.RequiresApproval || len(p.Steps) == 0 {
		t.Fatalf("plan = %+v", p)
	}
	if !hasPre(p.Steps[0], Precondition{Kind: PreApproved}) || p.Steps[0].Kind != StepStopAll {
		t.Errorf("first step = %+v", p.Steps[0])
	}
	var seq []string
	for _, s := range p.Steps {
		seq = append(seq, string(s.Kind)+":"+s.Slot)
	}
	all := append([]string{cand}, others...)
	var want []string
	for _, k := range []string{"stop-all", "await-stopped", "verify-identity", "snapshot-data"} {
		for _, s := range all {
			want = append(want, k+":"+s)
		}
	}
	want = append(want, "mark-safe-to-bootstrap:"+cand, "update-service:"+cand, "start-node:"+cand, "wait-synced:"+cand)
	for _, o := range others {
		want = append(want, "start-node:"+o, "wait-synced:"+o)
	}
	want = append(want, "update-service:"+cand, "await-update:"+cand, "wait-synced:"+cand, "check-recovered-state:")
	if !slices.Equal(seq, want) {
		t.Errorf("steps =\n%v\nwant\n%v", seq, want)
	}
	for i, s := range p.Steps {
		if i > 0 && hasPre(s, Precondition{Kind: PreApproved}) {
			t.Errorf("step %s repeats the approval", s.ID)
		}
		if s.Kind == StepMarkSafe || (s.Slot == cand && s.Kind == StepStartNode) || (s.Kind == StepUpdateService && strings.Contains(s.Note, "bootstrap spec")) {
			if !hasPre(s, Precondition{Kind: PreNoNodeRunning}) {
				t.Errorf("step %s without no-node-running", s.ID)
			}
		}
	}
}
