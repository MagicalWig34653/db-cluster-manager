// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const grastateZeroState = "version: 2.1\nuuid: " + galera.ZeroUUID + "\nseqno: -1\nsafe_to_bootstrap: 0\n"

type recordedNotes struct {
	mu sync.Mutex
	ev []notify.Event
}

func (r *recordedNotes) add(_ context.Context, e notify.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, e)
	return true
}

func (r *recordedNotes) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.ev {
		out = append(out, e.Kind)
	}
	return out
}

// zeroSetup builds the E12 state: three nodes with the zero-state
// grastate and the given wsrep-recover positions, and the health record of
// a zero-UUID Primary.
func zeroSetup(t *testing.T, seq map[string]int64, autoRecover bool) (*Service, *Operations, *healthAgents, *recordedNotes) {
	t.Helper()
	ha := &healthAgents{grastate: map[string]string{"h1": grastateZeroState, "h2": grastateZeroState, "h3": grastateZeroState},
		recovered: map[string]galera.Position{}}
	for h, n := range seq {
		ha.recovered[h] = galera.Position{UUID: galera.ZeroUUID, Seqno: n}
	}
	s, ops := recoverySetup(t, ha)
	notes := &recordedNotes{}
	ops.Notify = notes.add
	zeroHealth := galera.NodeHealth{State: galera.HealthUnhealthy, ClusterStatus: "Primary", ClusterSize: 3, Connected: true, Ready: true,
		StateUUID: galera.ZeroUUID, ReasonCode: galera.ReasonZeroUUIDPrimary}
	if _, _, err := Update(s.Store(), CollClusters, "clu-1", func(cr ClusterRecord, _ bool) (ClusterRecord, error) {
		cr.AutoRecover = autoRecover
		cr.Health = map[string]NodeObservation{}
		for _, sl := range []string{"n1", "n2", "n3"} {
			cr.Health[sl] = NodeObservation{Health: zeroHealth, ObservedAt: time.Now()}
		}
		return cr, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, ops, ha, notes
}

func opSucceeded(s *Service, id string) func() bool {
	return func() bool {
		v, err := s.Current().Runner.Snapshot(id)
		return err == nil && v.Status == runner.StatusSucceeded
	}
}

// drive runs the reconciler until the prepare operation has a follow-up.
func drive(t *testing.T, s *Service, ops *Operations, prepID string) OperationRecord {
	t.Helper()
	rec := ZeroUUIDReconciler{Ops: ops}
	var out OperationRecord
	waitFor(t, "follow-up of "+prepID, func() bool {
		_ = rec.ReconcileOnce(context.Background())
		r, ok, err := Get[OperationRecord](s.Store(), CollOperations, prepID)
		out = r.Value
		return err == nil && ok && out.FollowUp != ""
	})
	return out
}

func TestZeroUUIDRecoveryManualApprovalFlow(t *testing.T) {
	s, ops, ha, notes := zeroSetup(t, map[string]int64{"h1": 1303, "h2": 1304, "h3": 1302}, false)
	ctx := context.Background()
	// The operator proposes, a subject without rights cannot, only the
	// administrator approves.
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.StartZeroUUIDRecovery(ctx, nobody, "clu-1"); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized start: %v", err)
	}
	prep, err := ops.StartZeroUUIDRecovery(ctx, operatorSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if prep.Kind != api.OperationKindZeroUUIDPrepare || prep.Snapshot != "pre-recovery-"+prep.ID || prep.AutoApproved {
		t.Fatalf("prepare record %+v", prep)
	}
	if act, _ := approvalPermission(prep); act != authz.ActionRecover {
		t.Fatalf("approval permission %q", act)
	}
	if _, err := ops.StartZeroUUIDRecovery(ctx, operatorSubject, "clu-1"); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("second start: %v", err)
	}
	waitFor(t, "prepare waits", func() bool {
		v, err := s.Current().Runner.Snapshot(prep.ID)
		return err == nil && len(v.Steps) > 0
	})
	if k := kindsSent(ha); len(k) != 0 {
		t.Fatalf("commands before approval: %v", k)
	}
	if _, err := ops.Approve(operatorSubject, prep.ID, prep.ApprovalDigest); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("operator approved: %v", err)
	}
	if _, err := ops.Approve(adminSubject, prep.ID, prep.ApprovalDigest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prepare succeeded", opSucceeded(s, prep.ID))
	for _, k := range kindsSent(ha) {
		if k == agent.KindMarkSafeToBootstrap || k == agent.KindStartService {
			t.Fatalf("prepare phase sent %s", k)
		}
	}
	done := drive(t, s, ops, prep.ID)
	rec, ok, _ := Get[OperationRecord](s.Store(), CollOperations, done.FollowUp)
	if !ok || rec.Value.Kind != api.OperationKindZeroUUIDRecovery || rec.Value.AutoApproved || rec.Value.Snapshot != prep.Snapshot ||
		rec.Value.Diagnosis == nil || rec.Value.Diagnosis.Candidate != "n2" || !rec.Value.Diagnosis.AutoEligible {
		t.Fatalf("recovery record %+v", rec.Value)
	}
	// autoRecover is off: the eligible state still waits for a human.
	if _, ok := (StoreApprovals{Store: s.Store()}).Approval(rec.Value.ApprovalID); ok {
		t.Fatal("recovery approved without a human while autoRecover is off")
	}
	before := len(kindsSent(ha))
	time.Sleep(50 * time.Millisecond)
	if n := len(kindsSent(ha)); n != before {
		t.Fatalf("recovery progressed without approval: %v", kindsSent(ha)[before:])
	}
	if _, err := ops.Approve(adminSubject, rec.Value.ID, rec.Value.ApprovalDigest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recovery succeeded", opSucceeded(s, rec.Value.ID))
	checkZeroRecoveryCommands(t, ha, "h2", prep.Snapshot)
	// Notifications: before (phase 1 and 2) and after (both runs).
	rz := ZeroUUIDReconciler{Ops: ops}
	waitFor(t, "end notifications", func() bool {
		_ = rz.ReconcileOnce(ctx)
		return countOf(notes.kinds(), KindRecoveryEnded) >= 2
	})
	k := notes.kinds()
	if countOf(k, KindRecoveryNeedsHand) != 2 || k[0] != KindRecoveryNeedsHand {
		t.Fatalf("notifications %v", k)
	}
	_ = rz.ReconcileOnce(ctx)
	if n := countOf(notes.kinds(), KindRecoveryEnded); n != 2 {
		t.Fatalf("end notified %d times", n)
	}
	st, err := ops.ZeroUUIDStatus(adminSubject, "clu-1")
	if err != nil || st.Prepare == nil || st.Recovery == nil || st.Recovery.State != "succeeded" || st.Recovery.Candidate != "n2" || st.Recovery.AwaitingApproval {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := ops.ZeroUUIDStatus(nobody, "clu-1"); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("status without read permission: %v", err)
	}
}

func checkZeroRecoveryCommands(t *testing.T, ha *healthAgents, candidateHost, snapshot string) {
	t.Helper()
	snaps, marks := 0, 0
	lastSnap, markAt := -1, -1
	for i, c := range ha.commands() {
		switch c.Cmd.Kind {
		case agent.KindSnapshotData:
			var a agent.SnapshotDataArgs
			if err := json.Unmarshal(c.Cmd.Args, &a); err != nil || a.Name != snapshot {
				t.Fatalf("snapshot args %+v (%v)", a, err)
			}
			snaps++
			lastSnap = i
		case agent.KindMarkSafeToBootstrap:
			var a agent.MarkSafeArgs
			if err := json.Unmarshal(c.Cmd.Args, &a); err != nil || c.Target != candidateHost || !a.AllowZeroUUID || a.UUID != galera.ZeroUUID || a.Seqno != -1 {
				t.Fatalf("mark-safe to %s: %+v (%v)", c.Target, a, err)
			}
			marks++
			markAt = i
		}
	}
	if snaps < 3 || marks != 1 || lastSnap > markAt {
		t.Fatalf("snapshots %d (last at %d), mark-safe %d (at %d)", snaps, lastSnap, marks, markAt)
	}
}

func TestZeroUUIDAutoRecoverRunsWithoutApprovalWhenUnambiguous(t *testing.T) {
	s, ops, ha, notes := zeroSetup(t, map[string]int64{"h1": 1303, "h2": 1304, "h3": 1302}, true)
	ctx := context.Background()
	rz := ZeroUUIDReconciler{Ops: ops}
	if err := rz.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var prepID string
	waitFor(t, "auto prepare", func() bool {
		recs, _ := List[OperationRecord](s.Store(), CollOperations)
		for _, r := range recs {
			if r.Value.Kind == api.OperationKindZeroUUIDPrepare {
				prepID = r.Value.ID
				return r.Value.AutoApproved && r.Value.RequestedBy == AutoApprover
			}
		}
		return false
	})
	// A second tick does not start another run (busy, then cooldown).
	_ = rz.ReconcileOnce(ctx)
	waitFor(t, "prepare succeeded", opSucceeded(s, prepID))
	done := drive(t, s, ops, prepID)
	rec, _, _ := Get[OperationRecord](s.Store(), CollOperations, done.FollowUp)
	if !rec.Value.AutoApproved {
		t.Fatalf("eligible recovery not auto-approved: %+v", rec.Value.Diagnosis)
	}
	ap, ok := (StoreApprovals{Store: s.Store()}).Approval(rec.Value.ApprovalID)
	if !ok || ap.By != AutoApprover {
		t.Fatalf("approval %+v %v", ap, ok)
	}
	waitFor(t, "recovery succeeded", opSucceeded(s, rec.Value.ID))
	checkZeroRecoveryCommands(t, ha, "h2", done.Snapshot)
	waitFor(t, "end notifications", func() bool {
		_ = rz.ReconcileOnce(ctx)
		return countOf(notes.kinds(), KindRecoveryEnded) >= 2
	})
	k := notes.kinds()
	if k[0] != KindRecoveryPlanned || slices.Contains(k, KindRecoveryNeedsHand) {
		t.Fatalf("notifications %v", k)
	}
	notes.mu.Lock()
	for _, e := range notes.ev {
		if strings.Contains(e.Message, "password") {
			t.Fatalf("event leaks: %s", e.Message)
		}
	}
	notes.mu.Unlock()
	docs, _ := s.Store().RawList(CollOperations)
	if len(docs) != 2 {
		t.Fatalf("%d operations, want prepare and recovery", len(docs))
	}
}

func TestZeroUUIDAutoRecoverNeverCoversAmbiguity(t *testing.T) {
	cases := map[string]map[string]int64{
		"tie":        {"h1": 1304, "h2": 1304, "h3": 1302},
		"unreadable": {"h1": 1303, "h2": 1304},
	}
	for name, seq := range cases {
		t.Run(name, func(t *testing.T) {
			s, ops, ha, _ := zeroSetup(t, seq, true)
			rz := ZeroUUIDReconciler{Ops: ops}
			_ = rz.ReconcileOnce(context.Background())
			var prepID string
			waitFor(t, "auto prepare", func() bool {
				recs, _ := List[OperationRecord](s.Store(), CollOperations)
				for _, r := range recs {
					if r.Value.Kind == api.OperationKindZeroUUIDPrepare {
						prepID = r.Value.ID
					}
				}
				return prepID != ""
			})
			waitFor(t, "prepare succeeded", opSucceeded(s, prepID))
			done := drive(t, s, ops, prepID)
			rec, ok, _ := Get[OperationRecord](s.Store(), CollOperations, done.FollowUp)
			if !ok || rec.Value.AutoApproved || rec.Value.Diagnosis == nil || rec.Value.Diagnosis.AutoEligible {
				t.Fatalf("ambiguous state treated as eligible: %+v", rec.Value)
			}
			if _, ok := (StoreApprovals{Store: s.Store()}).Approval(rec.Value.ApprovalID); ok {
				t.Fatal("ambiguous recovery approved automatically")
			}
			time.Sleep(50 * time.Millisecond)
			for _, k := range kindsSent(ha) {
				if k == agent.KindMarkSafeToBootstrap {
					t.Fatal("bootstrap ran without approval")
				}
			}
		})
	}
}

func TestZeroUUIDPolicyOffStartsNothingAndIsAdminOnly(t *testing.T) {
	s, ops, ha, _ := zeroSetup(t, map[string]int64{"h1": 1, "h2": 2, "h3": 3}, false)
	rz := ZeroUUIDReconciler{Ops: ops}
	_ = rz.ReconcileOnce(context.Background())
	time.Sleep(30 * time.Millisecond)
	if docs, _ := s.Store().RawList(CollOperations); len(docs) != 0 || len(kindsSent(ha)) != 0 {
		t.Fatal("a run started with autoRecover off")
	}
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.SetAutoRecover(operatorSubject, "clu-1", true); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("operator switched the policy: %v", err)
	}
	if v, err := ops.SetAutoRecover(adminSubject, "clu-1", true); err != nil || !v {
		t.Fatalf("admin: %v %v", v, err)
	}
	cr, _, _ := Get[ClusterRecord](s.Store(), CollClusters, "clu-1")
	if !cr.Value.AutoRecover {
		t.Fatal("policy not stored")
	}
}

func TestZeroUUIDRefusedWhileARealPrimaryExists(t *testing.T) {
	s, ops, ha, _ := zeroSetup(t, map[string]int64{"h1": 1, "h2": 2, "h3": 3}, false)
	if _, _, err := Update(s.Store(), CollClusters, "clu-1", func(cr ClusterRecord, _ bool) (ClusterRecord, error) {
		h := cr.Health["n1"]
		h.Health.StateUUID = stateUUID
		cr.Health["n1"] = h
		return cr, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.StartZeroUUIDRecovery(context.Background(), adminSubject, "clu-1"); !errors.Is(err, galera.ErrNotFullOutage) {
		t.Fatalf("err = %v", err)
	}
	if docs, _ := s.Store().RawList(CollOperations); len(docs) != 0 || len(kindsSent(ha)) != 0 {
		t.Fatal("something started")
	}
}

func TestOutageDiagnosisOffersZeroUUIDRecovery(t *testing.T) {
	// Nodes stopped by hand (E12): zero-state files, recovered positions.
	ha := &healthAgents{grastate: map[string]string{"h1": grastateZeroState, "h2": grastateZeroState, "h3": grastateZeroState},
		recovered: map[string]galera.Position{"h1": {UUID: galera.ZeroUUID, Seqno: 1303}, "h2": {UUID: galera.ZeroUUID, Seqno: 1304}, "h3": {UUID: galera.ZeroUUID, Seqno: 1302}}}
	_, ops := recoverySetup(t, ha)
	c := &Control{Ops: ops}
	d, err := c.OutageDiagnosisRecover(context.Background(), adminSubject, "clu-1")
	if err != nil || d.Candidate != "n2" || d.RecoveryKind != "zero-uuid-recovery" || !d.AutoEligible {
		t.Fatalf("diagnosis %+v, %v", d, err)
	}
	// The read-only diagnosis never runs wsrep-recover: no candidate, but
	// the kind is named.
	d, err = c.OutageDiagnosis(context.Background(), adminSubject, "clu-1")
	if err != nil || d.Candidate != "" || d.RecoveryKind != "zero-uuid-recovery" {
		t.Fatalf("read-only diagnosis %+v, %v", d, err)
	}
}

func countOf(ks []string, k string) int {
	n := 0
	for _, x := range ks {
		if x == k {
			n++
		}
	}
	return n
}
