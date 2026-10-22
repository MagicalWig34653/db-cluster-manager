// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var testBackupSettings = BackupSettings{JobImage: "dbcm-job:1", BackendSecret: "dbcm-backend", DBCredentialSecret: "dbcm-backup-cred",
	RecipientKey: "age1recipient", KeySecret: "dbcm-recovery-key", InspectionNetwork: "dbcm-inspect-net"}

type backupEnv struct {
	svc    *Service
	ops    *Operations
	agents *fakeAgents
	clock  *testClock
	conn   *memConn
	be     *backup.MemBackend
	b      *Backups
}

// newBackupEnv starts one leader with the routing checker (galera
// conditions always hold, backup backend conditions are unmet) and a
// healthy three-node cluster "clu-1".
func newBackupEnv(t *testing.T, withBackupSettings bool) *backupEnv {
	t.Helper()
	env := &backupEnv{agents: newFakeAgents(), clock: &testClock{t: time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)},
		conn: &memConn{}, be: backup.NewMemBackend()}
	rc := &RoutingChecker{Cluster: okChecker{}}
	c := newClusterWith(t, 1, func(_ string, cfg *Config) {
		cfg.Agents, cfg.Checker, cfg.Now = env.agents, rc, env.clock.Now
	}, func(_ string, s *Service) { rc.Store, rc.Ops = s.Store(), &Operations{Svc: s} })
	env.svc = c.leader()
	ops, _ := replaceFixture(t, env.svc, 2)
	env.ops = ops
	env.freshHealth(t)
	if withBackupSettings {
		_, _, err := Update(env.svc.Store(), CollSettings, SettingsKey, func(s Settings, _ bool) (Settings, error) {
			s.Backup = testBackupSettings
			return s, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	env.b = &Backups{Ops: ops, Backend: env.be,
		Swarms: &Swarms{Svc: env.svc, Dial: func(string) (swarm.Connector, error) { return env.conn, nil }}}
	return env
}

// freshHealth records a healthy, synced observation of every node now.
func (env *backupEnv) freshHealth(t *testing.T) {
	t.Helper()
	_, _, err := Update(env.svc.Store(), CollClusters, "clu-1", func(cr ClusterRecord, _ bool) (ClusterRecord, error) {
		for _, slot := range []string{"n1", "n2", "n3"} {
			cr.Health[slot] = NodeObservation{ObservedAt: env.clock.Now(), Health: galera.NodeHealth{State: galera.HealthHealthy,
				LocalState: "Synced", ClusterStatus: "Primary", Ready: true, Connected: true, ClusterSize: 3,
				StateUUID: "11111111-1111-1111-1111-111111111111"}}
		}
		return cr, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// completedBackup stores a finished physical backup b1 of clu-1 with its
// manifest in the backend.
func (env *backupEnv) completedBackup(t *testing.T, id string, ended time.Time) {
	t.Helper()
	obj := dbbackup.ObjectName("clu-1", id, dbbackup.MethodPhysical, "")
	m := dbbackup.Manifest{Format: dbbackup.ManifestFormat, BackupID: id, Method: dbbackup.MethodPhysical, ClusterID: "clu-1",
		SourceNodeUUID: "00000000-0000-4000-8000-000000000002", Object: obj, Started: ended.Add(-time.Minute), Finished: ended,
		Stream: backup.StreamSummary{KeyID: "k", PlainSize: 10, CipherSize: 99, CipherSHA256: "aa",
			Chunks: []backup.ChunkDigest{{Index: 0, Size: 10, SHA256: "bb"}}},
		Stored: backup.PutResult{Size: 99, SHA256: "aa"}, Checkpoints: &dbbackup.Checkpoints{BackupType: "full-backuped", ToLSN: 5}}
	raw, _ := json.Marshal(m)
	if err := env.be.Put(context.Background(), dbbackup.ManifestName(obj), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	st := env.svc.Store()
	if _, err := Put(st, CollOperations, "op-"+id, OperationRecord{ID: "op-" + id, Kind: dbbackup.OpBackup, Swarm: "sw1", Cluster: "clu-1",
		View: &runner.View{ID: "op-" + id, Status: runner.StatusSucceeded}, RequestedAt: ended.Add(-time.Minute), EndedAt: ended}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollBackups, id, BackupRecord{ID: id, Cluster: "clu-1", Swarm: "sw1", OpID: "op-" + id,
		Method: dbbackup.MethodPhysical, Objects: []string{obj}}, nil); err != nil {
		t.Fatal(err)
	}
}

func (env *backupEnv) commandsFor(opID string) []agent.Command {
	var out []agent.Command
	for _, s := range env.agents.commands() {
		if s.Cmd.OpID == opID {
			out = append(out, s.Cmd)
		}
	}
	return out
}

func TestStartBackupPlansRecordsAndMapsErrors(t *testing.T) {
	env := newBackupEnv(t, false)
	if _, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1"); !errors.Is(err, api.ErrSettingsIncomplete) {
		t.Fatalf("without backup settings: %v", err)
	}
	env = newBackupEnv(t, true)
	if _, err := env.b.StartBackup(context.Background(), nobody, "clu-1"); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := env.b.StartBackup(context.Background(), adminSubject, "nope"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	ref, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Kind != dbbackup.OpBackup {
		t.Fatalf("%+v", ref)
	}
	recs, _ := List[BackupRecord](env.svc.Store(), CollBackups)
	if len(recs) != 1 || recs[0].Value.OpID != ref.ID || recs[0].Value.Trigger != "manual" || len(recs[0].Value.Objects) != 1 {
		t.Fatalf("backup record %+v", recs)
	}
	// The check-source gate needs the backend condition, which is unmet
	// without a backend checker: the operation pauses before any job.
	waitOpStatus(t, env.svc, ref.ID, runner.StatusPaused)
	for _, c := range env.commandsFor(ref.ID) {
		if c.Kind == agent.KindRunBackup {
			t.Fatal("backup job started although the source gate is unmet")
		}
	}
	if _, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1"); !errors.Is(err, api.ErrBusy) {
		t.Fatalf("second backup while the first is unfinished: %v", err)
	}
}

func TestBackupSummariesAndClusterFields(t *testing.T) {
	env := newBackupEnv(t, true)
	ended := env.clock.Now().Add(-time.Hour)
	env.completedBackup(t, "b1", ended)
	sums, err := env.b.Backups(context.Background())
	if err != nil || len(sums) != 1 {
		t.Fatalf("%v %+v", err, sums)
	}
	if !sums[0].LastBackupAt.Equal(ended) || sums[0].LastBackupID != "b1" || !sums[0].LastVerifiedRestoreAt.IsZero() || sums[0].Policy != "" {
		t.Fatalf("%+v", sums[0])
	}
	cl, err := Inventory{Store: env.svc.Store()}.Cluster(context.Background(), "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if !cl.LastBackupAt.Equal(ended) || !cl.LastVerifiedRestoreAt.IsZero() || cl.AutomationPolicy != "manual" || len(cl.Endpoints) != 3 ||
		cl.Endpoints[0].Host != "dbcm-shop-n1" || cl.Endpoints[0].Port != 3306 {
		t.Fatalf("%+v", cl)
	}
}

func TestBackupPolicyScheduleRunsAsSystemSubject(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "not a schedule"}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("bad schedule: %v", err)
	}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *", Method: "logical"}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("logical schedule: %v", err)
	}
	if _, err := env.b.SetBackupPolicy(ctx, nobody, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *"}); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	sum, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Policy != "0 2 * * *" || !sum.NextRunAt.Equal(time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)) || sum.BackupPolicy.Missed != "skip" {
		t.Fatalf("%+v", sum)
	}
	// Nothing is due yet.
	env.b.RunSchedules(collect.SystemSubject)
	if recs, _ := List[BackupRecord](env.svc.Store(), CollBackups); len(recs) != 0 {
		t.Fatal("backup before the scheduled time")
	}
	// Due, but the system subject is not bound yet: fail closed, the due
	// time is consumed and nothing starts.
	env.clock.Set(time.Date(2026, 10, 22, 2, 1, 0, 0, time.UTC))
	env.b.RunSchedules(collect.SystemSubject)
	if recs, _ := List[BackupRecord](env.svc.Store(), CollBackups); len(recs) != 0 {
		t.Fatal("scheduled backup started without permission")
	}
	added, err := EnsureSystemBinding(env.ops.Authorizer.Policy())
	if err != nil || !added {
		t.Fatalf("binding: %v %v", added, err)
	}
	if added, err := EnsureSystemBinding(env.ops.Authorizer.Policy()); err != nil || added {
		t.Fatalf("second binding: %v %v", added, err)
	}
	env.clock.Set(time.Date(2026, 10, 23, 2, 0, 30, 0, time.UTC))
	env.freshHealth(t)
	env.b.RunSchedules(collect.SystemSubject)
	recs, _ := List[BackupRecord](env.svc.Store(), CollBackups)
	if len(recs) != 1 || recs[0].Value.Trigger != "schedule" || recs[0].Value.By != collect.SystemSubject.ID {
		t.Fatalf("scheduled backup %+v", recs)
	}
	// The same due time does not start a second run.
	env.b.RunSchedules(collect.SystemSubject)
	if recs, _ := List[BackupRecord](env.svc.Store(), CollBackups); len(recs) != 1 {
		t.Fatalf("duplicate scheduled run: %d", len(recs))
	}
}

func TestInspectionWaitsForDigestBoundApprovalAndExpires(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	if _, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("without a completed backup: %v", err)
	}
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))
	if _, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1"}); !errors.Is(err, api.ErrNoCapacity) {
		t.Fatalf("automatic host without disk observations: %v", err)
	}
	if _, err := env.b.StartInspection(ctx, nobody, api.InspectionInput{Cluster: "clu-1", Host: "h4"}); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	res, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", TTLMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup != "b1" || res.Host != "h4" || len(res.Approval.Digest) != 64 || res.InspectionPassword == "" || res.Kind != dbbackup.OpInspection {
		t.Fatalf("%+v", res)
	}
	secret := env.conn.secrets["dbcm-inspect-"+res.Session+"-init"]
	if !strings.Contains(string(secret), res.InspectionPassword) {
		t.Fatal("init secret not created with the inspection password")
	}
	for _, d := range mustRaw(t, env.svc, CollOperations, CollInspections) {
		if strings.Contains(string(d), res.InspectionPassword) {
			t.Fatal("inspection password stored in the replicated store")
		}
	}
	// Every destructive step waits for the approval: nothing is sent.
	waitOpStatus(t, env.svc, res.ID, runner.StatusPaused)
	if cmds := env.commandsFor(res.ID); len(cmds) != 0 {
		t.Fatalf("commands before approval: %+v", cmds)
	}
	ops, _ := Inventory{Store: env.svc.Store()}.Operations(ctx)
	var listed api.Operation
	for _, o := range ops {
		if o.ID == res.ID {
			listed = o
		}
	}
	if listed.Approval == nil || listed.Approval.Digest != res.Approval.Digest || listed.Approval.ApprovedBy != "" || listed.Reason == "" {
		t.Fatalf("listed %+v", listed)
	}

	ctl := &Control{Ops: env.ops, Swarms: env.b.Swarms}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, res.ID, strings.Repeat("0", 64)); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, nobody, res.ID, res.Approval.Digest); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized approval: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, "op-b1", res.Approval.Digest); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("operation without approval: %v", err)
	}
	a, err := ctl.ApproveOperation(ctx, adminSubject, res.ID, res.Approval.Digest)
	if err != nil || a.ApprovedBy != "admin" {
		t.Fatalf("%+v %v", a, err)
	}
	waitFor(t, "create-volume after approval", func() bool {
		for _, c := range env.commandsFor(res.ID) {
			if c.Kind == agent.KindCreateVolume {
				return true
			}
		}
		return false
	})
	// The restore job's completion is a backend condition: the operation
	// pauses there (no backend checker in this test).
	waitFor(t, "paused at the restore gate", func() bool {
		r := opRecord(t, env.svc, res.ID)
		return r.View != nil && r.View.Status == runner.StatusPaused && len(env.commandsFor(res.ID)) >= 3
	})

	// The approval is bound to the stored definition: a changed definition
	// is no longer approved.
	appr := StoreApprovals{Store: env.svc.Store()}
	rec := opRecord(t, env.svc, res.ID)
	if _, ok := appr.Approval(rec.ApprovalID); !ok {
		t.Fatal("approval not found")
	}
	tampered := rec
	tampered.Definition.Steps = append([]runner.Step(nil), rec.Definition.Steps...)
	tampered.Definition.Steps[0].Note = "changed"
	if _, err := Put(env.svc.Store(), CollOperations, "op-tamper", OperationRecord{ID: "op-tamper", Definition: tampered.Definition,
		ApprovalID: "apr-tamper", ApprovalDigest: rec.ApprovalDigest}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(env.svc.Store(), CollApprovals, "apr-tamper", ApprovalRecord{ID: "apr-tamper", OpID: "op-tamper",
		Digest: rec.ApprovalDigest, By: "admin"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := appr.Approval("apr-tamper"); ok {
		t.Fatal("approval accepted for a different definition")
	}

	// Expiry: the unfinished inspection is aborted, then torn down.
	env.clock.Set(env.clock.Now().Add(31 * time.Minute))
	env.b.TeardownExpired(collect.SystemSubject)
	waitFor(t, "inspection aborted", func() bool { return !opRecord(t, env.svc, res.ID).EndedAt.IsZero() })
	env.b.TeardownExpired(collect.SystemSubject)
	ir, _, _ := Get[InspectionRecord](env.svc.Store(), CollInspections, res.Session)
	if ir.Value.TeardownOp == "" {
		t.Fatal("teardown not started")
	}
	waitOpStatus(t, env.svc, ir.Value.TeardownOp, runner.StatusSucceeded)
	kinds := map[agent.Kind]bool{}
	for _, c := range env.commandsFor(ir.Value.TeardownOp) {
		kinds[c.Kind] = true
	}
	if !kinds[agent.KindRemoveVolume] || !kinds[agent.KindRemoveJob] {
		t.Fatalf("teardown commands %v", kinds)
	}
}

func mustRaw(t *testing.T, s *Service, colls ...string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, c := range colls {
		docs, err := s.Store().RawList(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			out = append(out, d.Value)
		}
	}
	return out
}

func TestNotificationSettingsSealSecretsAndTestDelivery(t *testing.T) {
	env := newBackupEnv(t, false)
	ctx := context.Background()
	var mu sync.Mutex
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	mon, err := NewMonitoring(env.svc, MonitoringConfig{Keys: testKey})
	if err != nil {
		t.Fatal(err)
	}
	ss := &SettingsService{Svc: env.svc, Authorizer: env.ops.Authorizer, Keys: testKey, Monitoring: mon}
	secret := "hook-secret-value"
	allow := true
	in := api.NotificationSettingsInput{
		Webhook: api.WebhookSettingsInput{Enabled: true, URL: srv.URL, Secret: &secret, AllowPrivate: &allow},
		Email:   api.EmailSettingsInput{Enabled: false, Recipients: []string{}},
	}
	if _, err := ss.SetNotificationSettings(ctx, nobody, in); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	view, err := ss.SetNotificationSettings(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Webhook.Enabled || !view.Webhook.SecretSet || view.Webhook.URL != srv.URL || view.Email.Enabled || view.MinSeverity != "warning" {
		t.Fatalf("%+v", view)
	}
	for _, d := range mustRaw(t, env.svc, CollSettings, CollNotifySecrets) {
		if strings.Contains(string(d), secret) {
			t.Fatal("webhook secret stored in plain text")
		}
	}
	// A later update without a secret keeps the stored one.
	in.Webhook.Secret = nil
	in.MinSeverity = "error"
	if view, err = ss.SetNotificationSettings(ctx, adminSubject, in); err != nil || !view.Webhook.SecretSet || view.MinSeverity != "error" {
		t.Fatalf("%+v %v", view, err)
	}
	res, err := ss.TestNotification(ctx, adminSubject, "webhook")
	if err != nil || !res.Delivered {
		t.Fatalf("%+v %v", res, err)
	}
	mu.Lock()
	if len(got) != 1 || got[0].Get(notify.HeaderSignature) == "" {
		t.Fatalf("webhook not signed: %v", got)
	}
	mu.Unlock()
	if _, err := ss.TestNotification(ctx, adminSubject, "email"); !errors.Is(err, api.ErrSettingsIncomplete) {
		t.Fatalf("email not enabled: %v", err)
	}
	// Enabling email without an SMTP address is invalid.
	in.Email.Enabled, in.Email.Recipients = true, []string{"ops@example.test"}
	if _, err := ss.SetNotificationSettings(ctx, adminSubject, in); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("email without addr: %v", err)
	}
	addr, from := "smtp.example.test:587", "dbcm@example.test"
	in.Email.Addr, in.Email.From = &addr, &from
	if view, err = ss.SetNotificationSettings(ctx, adminSubject, in); err != nil || !view.Email.Enabled || view.Email.PasswordSet || view.Email.Addr != addr {
		t.Fatalf("%+v %v", view, err)
	}
	// Clearing the secret removes it; an enabled webhook then fails validation.
	empty := ""
	in.Webhook.Secret = &empty
	if _, err := ss.SetNotificationSettings(ctx, adminSubject, in); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("enabled webhook without secret: %v", err)
	}
	in.Webhook.Enabled = false
	if view, err = ss.SetNotificationSettings(ctx, adminSubject, in); err != nil || view.Webhook.SecretSet || view.Webhook.Enabled {
		t.Fatalf("%+v %v", view, err)
	}
}

func TestControlAdapterMapsErrorsAndKeepsAgentURL(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents })
	l := c.leader()
	ops, _ := replaceFixture(t, l, 1)
	if _, _, err := Update(l.Store(), CollHosts, HostKey("sw1", "h1"), func(h HostRecord, _ bool) (HostRecord, error) {
		h.AgentURL = "http://10.0.0.1:7443"
		return h, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctl := &Control{Ops: ops, Swarms: &Swarms{}}
	ctx := context.Background()
	if _, err := ctl.ReplaceNode(ctx, adminSubject, "clu-1", "n3"); !errors.Is(err, api.ErrRequiresGuidedRecovery) {
		t.Fatalf("got %v", err)
	}
	if _, err := ctl.ReplaceNode(ctx, nobody, "clu-1", "n3"); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	req := api.CreateClusterRequest(createReq())
	req.Name = "other"
	if _, err := ctl.CreateCluster(ctx, adminSubject, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ctl.CreateCluster(ctx, adminSubject, req); !errors.Is(err, api.ErrClusterExists) {
		t.Fatalf("duplicate: %v", err)
	}
	h, err := ctl.SetEnrollment(ctx, adminSubject, "sw1", "h1", true)
	if err != nil || h.Enrollment != "enrolled" {
		t.Fatalf("%+v %v", h, err)
	}
	cur, _, _ := Get[HostRecord](l.Store(), CollHosts, HostKey("sw1", "h1"))
	if cur.Value.AgentURL != "http://10.0.0.1:7443" {
		t.Fatal("lab agent URL lost on re-enrollment")
	}
	hosts, err := ctl.Hosts(ctx)
	if err != nil || len(hosts) != 4 {
		t.Fatalf("%v %d", err, len(hosts))
	}
	if err := ctl.SyncSwarm(ctx, adminSubject, "sw1"); !errors.Is(err, api.ErrSettingsIncomplete) {
		t.Fatalf("sync without connector: %v", err)
	}
	if err := ctl.AbortOperation(ctx, adminSubject, "nope"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("abort unknown: %v", err)
	}
}

func TestMapCommandError(t *testing.T) {
	cases := map[error]error{
		galera.ErrNoCapacity:             api.ErrNoCapacity,
		galera.ErrPolicyConflict:         api.ErrPolicyConflict,
		ErrSettingsIncomplete:            api.ErrSettingsIncomplete,
		ErrClusterExists:                 api.ErrClusterExists,
		runner.ErrBusy:                   api.ErrBusy,
		ErrNotLeader:                     api.ErrNotLeader,
		runner.ErrNotLeader:              api.ErrNotLeader,
		ErrApprovalMismatch:              api.ErrApprovalMismatch,
		galera.ErrRequiresGuidedRecovery: api.ErrRequiresGuidedRecovery,
		ErrNoApproval:                    api.ErrConflict,
		dbbackup.ErrSchedule:             api.ErrInvalidInput,
		&authz.DeniedError{}:             api.ErrForbidden,
	}
	for in, want := range cases {
		if got := MapCommandError(in); !errors.Is(got, want) {
			t.Errorf("%v -> %v, want %v", in, got, want)
		}
	}
	other := errors.New("disk on fire")
	if MapCommandError(other) != other || MapCommandError(nil) != nil {
		t.Fatal("unknown errors must pass through")
	}
}

// TestBackupSourceGatePassesWithBackend: lab E4 found every backup paused
// at check-source ("job CPU and memory demand required") because the
// re-check lacked the job demand the planning used. With a backend the
// gate now passes and the job is started.
func TestBackupSourceGatePassesWithBackend(t *testing.T) {
	env := newBackupEnv(t, true)
	rc := env.svc.cfg.Checker.(*RoutingChecker)
	rc.Backup = &dbbackup.Checker{Backend: env.be}
	ref, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run-backup sent", func() bool {
		return slices.ContainsFunc(env.commandsFor(ref.ID), func(c agent.Command) bool { return c.Kind == agent.KindRunBackup })
	})
}
