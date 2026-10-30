// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// approvalChecker evaluates CondApproved from the store (like
// RoutingChecker) and accepts every other condition.
type approvalChecker struct{ svc *Service }

func (a *approvalChecker) Check(_ context.Context, _ string, _ runner.Step, c runner.Condition) (bool, string, error) {
	if c.Kind == runner.CondApproved {
		ap, ok := StoreApprovals{Store: a.svc.Store()}.Approval(c.Target)
		return ok && ap.By != "", "not approved", nil
	}
	return true, "", nil
}

func recoverySetup(t *testing.T, ha *healthAgents) (*Service, *Operations) {
	t.Helper()
	chk := &approvalChecker{}
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = ha; cfg.Checker = chk })
	s := c.leader()
	chk.svc = s
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm"}, nil); err != nil {
		t.Fatal(err)
	}
	spec := galera.Cluster{InstallationID: "inst-1", ClusterID: "clu-1", Name: "shop", Members: []string{"n1", "n2", "n3"},
		Image:   "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873",
		Network: "dbcm-shop-galera", RootPasswordSecret: "dbcm-shop-root-pw"}
	cr := ClusterRecord{ID: "clu-1", Swarm: "sw1", Name: "shop", Size: 3, Spec: spec, Nodes: []galera.Node{
		{Slot: "n1", NodeUUID: nodeUUID1, HostID: "h1", Volume: "shop-n1"},
		{Slot: "n2", NodeUUID: nodeUUID2, HostID: "h2", Volume: "shop-n2"},
		{Slot: "n3", NodeUUID: nodeUUID3, HostID: "h3", Volume: "shop-n3"},
	}, ServiceID: map[string]string{"n1": "svc-1", "n2": "svc-2", "n3": "svc-3"}}
	if _, err := Put(s.Store(), CollClusters, cr.ID, cr, nil); err != nil {
		t.Fatal(err)
	}
	return s, opsFor(t, s)
}

func gstate(seqno string, safe string) string {
	return "version: 2.1\nuuid: " + stateUUID + "\nseqno: " + seqno + "\nsafe_to_bootstrap: " + safe + "\n"
}

func kindsSent(ha *healthAgents) []agent.Kind {
	var out []agent.Kind
	for _, c := range ha.commands() {
		if c.Cmd.Kind != agent.KindReadGrastate && c.Cmd.Kind != agent.KindCollectHealth && c.Cmd.Kind != agent.KindWsrepRecover {
			out = append(out, c.Cmd.Kind)
		}
	}
	return out
}

func TestRecoverOutageWaitsForApprovalThenRunsTheGuidedPlan(t *testing.T) {
	ha := &healthAgents{grastate: map[string]string{"h1": gstate("120", "0"), "h2": gstate("125", "0"), "h3": gstate("123", "0")}}
	s, ops := recoverySetup(t, ha)
	ctx := context.Background()
	if _, _, err := ops.RecoverOutage(ctx, nobody, "clu-1", "n2"); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized: %v", err)
	}
	// The user must confirm the candidate the diagnosis proposes.
	if _, d, err := ops.RecoverOutage(ctx, adminSubject, "clu-1", "n1"); !errors.Is(err, ErrCandidateMismatch) || d.Candidate != "n2" {
		t.Fatalf("other candidate: %v (%+v)", err, d)
	}
	if docs, _ := s.Store().RawList(CollOperations); len(docs) != 0 {
		t.Fatal("a refused request stored an operation")
	}
	rec, d, err := ops.RecoverOutage(ctx, adminSubject, "clu-1", "n2")
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "n2" || rec.ApprovalID == "" || rec.ApprovalDigest == "" || rec.Kind != "galera/full-outage-recovery" {
		t.Fatalf("record %+v diagnosis %+v", rec, d)
	}
	if want, _ := DefinitionDigest(rec.Definition); want != rec.ApprovalDigest {
		t.Fatal("digest does not cover the stored definition")
	}
	// Nothing is stopped, marked or started before the approval.
	waitFor(t, "operation waits", func() bool {
		v, err := s.Current().Runner.Snapshot(rec.ID)
		return err == nil && len(v.Steps) > 0
	})
	if k := kindsSent(ha); len(k) != 0 {
		t.Fatalf("commands before approval: %v", k)
	}
	if _, _, err := ops.RecoverOutage(ctx, adminSubject, "clu-1", "n2"); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("second request while one waits: %v", err)
	}
	// Approving needs the separate recover permission: neither operate
	// (operator preset) nor restore on the cluster's backups is enough.
	restorer := authz.Subject{ID: "restorer", Kind: authz.SubjectUser}
	pol := ops.Authorizer.Policy()
	if err := pol.AddRole(authz.Role{Name: "restore-only", Permissions: []authz.Permission{
		{Type: authz.ResourceBackup, Action: authz.ActionRestore}, {Type: authz.ResourceCluster, Action: authz.ActionOperate}}}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []authz.Binding{{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global},
		{Subject: restorer, Role: "restore-only", Scope: authz.Global}} {
		if err := pol.Bind(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, who := range []authz.Subject{operatorSubject, restorer} {
		if _, err := ops.Approve(who, rec.ID, rec.ApprovalDigest); !errors.As(err, new(*authz.DeniedError)) {
			t.Fatalf("approval by %s: %v", who.ID, err)
		}
	}
	if act, _ := approvalPermission(rec); act != authz.ActionRecover {
		t.Fatalf("approval permission %q", act)
	}
	if _, err := ops.Approve(adminSubject, rec.ID, "0000"); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	if _, err := ops.Approve(adminSubject, rec.ID, rec.ApprovalDigest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "recovery succeeded", func() bool {
		v, err := s.Current().Runner.Snapshot(rec.ID)
		return err == nil && v.Status == runner.StatusSucceeded
	})
	got := kindsSent(ha)
	want := []agent.Kind{agent.KindStopDB, agent.KindStopDB, agent.KindStopDB,
		agent.KindAwaitStopped, agent.KindAwaitStopped, agent.KindAwaitStopped,
		agent.KindVerifyIdentity, agent.KindVerifyIdentity, agent.KindVerifyIdentity,
		agent.KindSnapshotData, agent.KindSnapshotData, agent.KindSnapshotData,
		agent.KindMarkSafeToBootstrap, agent.KindUpdateService, agent.KindStartService,
		agent.KindStartService, agent.KindStartService, agent.KindUpdateService, agent.KindAwaitService}
	if !slices.Equal(got, want) {
		t.Fatalf("commands\n%v\nwant\n%v", got, want)
	}
	for _, c := range ha.commands() {
		if c.Cmd.Kind != agent.KindMarkSafeToBootstrap {
			continue
		}
		var a agent.MarkSafeArgs
		if err := json.Unmarshal(c.Cmd.Args, &a); err != nil || c.Target != "h2" || a.Seqno != 125 || a.UUID != stateUUID || a.Expected.NodeUUID != nodeUUID2 {
			t.Fatalf("mark-safe to %s: %+v (%v)", c.Target, a, err)
		}
	}
}

func TestRecoverOutageWithoutCandidateStartsNothing(t *testing.T) {
	// All nodes crashed: seqno -1 everywhere and no wsrep-recover results.
	ha := &healthAgents{grastate: map[string]string{"h1": gstate("-1", "0"), "h2": gstate("-1", "0"), "h3": gstate("-1", "0")}}
	s, ops := recoverySetup(t, ha)
	if _, d, err := ops.RecoverOutage(context.Background(), adminSubject, "clu-1", "n1"); !errors.Is(err, galera.ErrNoCandidate) || d.Candidate != "" {
		t.Fatalf("no candidate: %v (%+v)", err, d)
	}
	if docs, _ := s.Store().RawList(CollOperations); len(docs) != 0 || len(kindsSent(ha)) != 0 {
		t.Fatal("something was started without a candidate")
	}
	// It tried to recover every crashed node's position first.
	n := 0
	for _, c := range ha.commands() {
		if c.Cmd.Kind == agent.KindWsrepRecover {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("wsrep-recover sent %d times", n)
	}
}

func TestRecoverOutageAfterCrashUsesRecoveredPositions(t *testing.T) {
	ha := &healthAgents{
		grastate:  map[string]string{"h1": gstate("-1", "0"), "h2": gstate("-1", "0"), "h3": gstate("-1", "0")},
		recovered: map[string]galera.Position{"h1": {UUID: stateUUID, Seqno: 3525}, "h2": {UUID: stateUUID, Seqno: 3527}, "h3": {UUID: stateUUID, Seqno: 3526}},
	}
	s, ops := recoverySetup(t, ha)
	ctx := context.Background()
	// The read-only diagnosis never runs wsrep-recover.
	d, _, err := ops.DiagnoseOutageCollected(ctx, adminSubject, "clu-1")
	if err != nil || d.Candidate != "" {
		t.Fatalf("read-only diagnosis %+v, %v", d, err)
	}
	for _, c := range ha.commands() {
		if c.Cmd.Kind == agent.KindWsrepRecover {
			t.Fatal("GET diagnosis ran wsrep-recover")
		}
	}
	if _, _, err := ops.DiagnoseOutageRecovering(ctx, nobody, "clu-1"); err == nil {
		t.Fatal("wsrep-recover without operate permission")
	}
	d, ev, err := ops.DiagnoseOutageRecovering(ctx, adminSubject, "clu-1")
	if err != nil || d.Candidate != "n2" || ev[1].Recovered == nil || ev[1].Recovered.Seqno != 3527 {
		t.Fatalf("recovering diagnosis %+v %+v, %v", d, ev, err)
	}
	for _, c := range ha.commands() {
		if c.Cmd.Kind != agent.KindWsrepRecover {
			continue
		}
		var a agent.WsrepRecoverArgs
		if err := json.Unmarshal(c.Cmd.Args, &a); err != nil || a.Image == "" || a.UID <= 0 || a.Volume == "" || a.Expected.ClusterID != "clu-1" {
			t.Fatalf("wsrep-recover args %+v (%v)", a, err)
		}
	}
	rec, _, err := ops.RecoverOutage(ctx, adminSubject, "clu-1", "n2")
	if err != nil {
		t.Fatal(err)
	}
	// mark-safe binds to the file as read: seqno -1.
	for _, st := range rec.Definition.Steps {
		if st.Command != agent.KindMarkSafeToBootstrap {
			continue
		}
		var a agent.MarkSafeArgs
		if err := json.Unmarshal(st.Args, &a); err != nil || a.Seqno != -1 || a.UUID != stateUUID || st.Target != "h2" ||
			a.SetSeqno == nil || *a.SetSeqno != 3527 {
			t.Fatalf("mark-safe %+v (%v)", a, err)
		}
	}
	_ = s
}

func TestRecoverPositionsRefusedWhileAPrimaryWasSeen(t *testing.T) {
	ha := &healthAgents{grastate: map[string]string{"h1": gstate("-1", "0"), "h2": gstate("-1", "0"), "h3": gstate("-1", "0")}}
	s, ops := recoverySetup(t, ha)
	if _, _, err := Update(s.Store(), CollClusters, "clu-1", func(cr ClusterRecord, _ bool) (ClusterRecord, error) {
		cr.Health = map[string]NodeObservation{"n1": {Health: galera.NodeHealth{State: galera.HealthHealthy, ClusterStatus: "Primary", ClusterSize: 3, Connected: true, Ready: true}}}
		return cr, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ops.DiagnoseOutageRecovering(context.Background(), adminSubject, "clu-1"); !errors.Is(err, galera.ErrNotFullOutage) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range ha.commands() {
		if c.Cmd.Kind == agent.KindWsrepRecover {
			t.Fatal("wsrep-recover ran while a Primary was seen")
		}
	}
}
