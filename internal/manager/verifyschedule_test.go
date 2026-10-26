// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

func verifyEnv(t *testing.T) (*backupEnv, *notes) {
	t.Helper()
	env := newBackupEnv(t, true)
	n := &notes{}
	env.b.Notify = n.Notify
	if _, err := EnsureSystemBinding(env.ops.Authorizer.Policy()); err != nil {
		t.Fatal(err)
	}
	pol := api.BackupPolicy{Schedule: "0 2 * * *", Verify: &api.RestoreVerifyPolicy{Enabled: true, Schedule: "0 4 * * *", MaxMinutes: 60, Host: "h4"}}
	if _, err := env.b.SetBackupPolicy(context.Background(), adminSubject, "clu-1", pol); err != nil {
		t.Fatal(err)
	}
	return env, n
}

func verifyRun(t *testing.T, env *backupEnv) VerifyRunRecord {
	t.Helper()
	r, ok, err := Get[VerifyRunRecord](env.svc.Store(), CollVerifyRuns, "clu-1", Consistent())
	if err != nil || !ok {
		t.Fatalf("no verification run: %v", err)
	}
	return r.Value
}

func TestScheduledVerificationPassesAndTearsDown(t *testing.T) {
	env, n := verifyEnv(t)
	ctx := context.Background()
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))

	// Not due yet.
	env.b.RunVerifications(ctx, collect.SystemSubject)
	if _, ok, _ := Get[VerifyRunRecord](env.svc.Store(), CollVerifyRuns, "clu-1"); ok {
		t.Fatal("verification before its time")
	}
	env.clock.Set(time.Date(2026, 10, 22, 4, 1, 0, 0, time.UTC))
	env.freshHealth(t)
	env.b.RunVerifications(ctx, collect.SystemSubject)
	run := verifyRun(t, env)
	if run.Status != VerifyRunning || run.Backup != "b1" || run.Session == "" {
		t.Fatalf("run %+v", run)
	}
	// The same due time does not start a second run.
	env.b.RunVerifications(ctx, collect.SystemSubject)
	if again := verifyRun(t, env); again.OpID != run.OpID {
		t.Fatal("second run started for the same due time")
	}
	// The approval was recorded for the digest and names the policy author.
	op := opRecord(t, env.svc, run.OpID)
	ap, ok, _ := Get[ApprovalRecord](env.svc.Store(), CollApprovals, op.ApprovalID)
	if !ok || ap.Value.Digest != op.ApprovalDigest || !strings.Contains(ap.Value.By, "admin") {
		t.Fatalf("approval %+v", ap)
	}
	waitFor(t, "inspection started without a person", func() bool { return len(env.commandsFor(run.OpID)) > 0 })

	// The gate records a passing report: the run passes, the instance expires now.
	if _, err := Put(env.svc.Store(), CollVerifications, run.Session, dbbackup.VerifyReport{BackupID: "b1", ClusterID: "clu-1", SessionID: run.Session,
		Level: dbbackup.VerifyLevelInstance, At: env.clock.Now(), Passed: true}, nil); err != nil {
		t.Fatal(err)
	}
	env.b.RunVerifications(ctx, collect.SystemSubject)
	if got := verifyRun(t, env); got.Status != VerifyPassed {
		t.Fatalf("run %+v", got)
	}
	ir, _, _ := Get[InspectionRecord](env.svc.Store(), CollInspections, run.Session)
	if ir.Value.Session.ExpiresAt.After(env.clock.Now()) {
		t.Fatalf("instance not expired: %v", ir.Value.Session.ExpiresAt)
	}
	if k := n.kinds(); len(k) != 1 || k[0] != KindRestoreVerifyPassed {
		t.Fatalf("notifications %v", k)
	}
}

func TestScheduledVerificationFailureNotifies(t *testing.T) {
	env, n := verifyEnv(t)
	ctx := context.Background()
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))
	env.clock.Set(time.Date(2026, 10, 22, 4, 1, 0, 0, time.UTC))
	env.freshHealth(t)
	env.b.RunVerifications(ctx, collect.SystemSubject)
	run := verifyRun(t, env)
	if _, err := Put(env.svc.Store(), CollVerifications, run.Session, dbbackup.VerifyReport{BackupID: "b1", ClusterID: "clu-1", SessionID: run.Session,
		Level: dbbackup.VerifyLevelInstance, At: env.clock.Now(), Passed: false}, nil); err != nil {
		t.Fatal(err)
	}
	env.b.RunVerifications(ctx, collect.SystemSubject)
	got := verifyRun(t, env)
	if got.Status != VerifyFailed || !strings.Contains(got.Reason, "failed the checks") {
		t.Fatalf("run %+v", got)
	}
	if k := n.kinds(); len(k) != 1 || k[0] != KindRestoreVerifyFailed {
		t.Fatalf("notifications %v", k)
	}
	if n.ev[0].Severity != notify.SeverityCritical || n.ev[0].Details["backup"] != "b1" {
		t.Fatalf("event %+v", n.ev[0])
	}
}

func TestScheduledVerificationOverdueIsAbortedAndNotified(t *testing.T) {
	env, n := verifyEnv(t)
	ctx := context.Background()
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))
	env.clock.Set(time.Date(2026, 10, 22, 4, 1, 0, 0, time.UTC))
	env.freshHealth(t)
	env.b.RunVerifications(ctx, collect.SystemSubject)
	run := verifyRun(t, env)
	// Within the deadline nothing happens.
	env.clock.Set(run.StartedAt.Add(30 * time.Minute))
	env.b.RunVerifications(ctx, collect.SystemSubject)
	if verifyRun(t, env).Status != VerifyRunning || len(n.kinds()) != 0 {
		t.Fatal("run ended before its deadline")
	}
	env.clock.Set(run.StartedAt.Add(61 * time.Minute))
	env.b.RunVerifications(ctx, collect.SystemSubject)
	got := verifyRun(t, env)
	if got.Status != VerifyFailed || !strings.Contains(got.Reason, "aborted") {
		t.Fatalf("run %+v", got)
	}
	waitFor(t, "operation aborted", func() bool { return !opRecord(t, env.svc, run.OpID).EndedAt.IsZero() })
	if k := n.kinds(); len(k) != 1 || k[0] != KindRestoreVerifyFailed {
		t.Fatalf("notifications %v", k)
	}
	// The expired instance is torn down by the regular expiry pass.
	env.b.TeardownExpired(collect.SystemSubject)
	ir, _, _ := Get[InspectionRecord](env.svc.Store(), CollInspections, run.Session)
	if ir.Value.TeardownOp == "" {
		t.Fatal("teardown not started for the aborted verification")
	}
}

func TestScheduledVerificationNotStartedIsAFailure(t *testing.T) {
	env, n := verifyEnv(t)
	// No completed backup: the inspection cannot be planned.
	env.clock.Set(time.Date(2026, 10, 22, 4, 1, 0, 0, time.UTC))
	env.b.RunVerifications(context.Background(), collect.SystemSubject)
	got := verifyRun(t, env)
	if got.Status != VerifyFailed || !strings.Contains(got.Reason, "not started") {
		t.Fatalf("run %+v", got)
	}
	if k := n.kinds(); len(k) != 1 || k[0] != KindRestoreVerifyFailed {
		t.Fatalf("notifications %v", k)
	}
	if ops, _ := env.svc.Store().RawList(CollInspections); len(ops) != 0 {
		t.Fatal("inspection record created for a failed start")
	}
}

// grantBackupUpdateOnly binds a custom role that may update backup policies
// but not restore.
func grantBackupUpdateOnly(t *testing.T, env *backupEnv, sub authz.Subject) {
	t.Helper()
	pol := env.ops.Authorizer.Policy()
	if err := pol.AddRole(authz.Role{Name: "backup-updater", Permissions: []authz.Permission{
		{Type: authz.ResourceBackup, Action: authz.ActionUpdate}, {Type: authz.ResourceBackup, Action: authz.ActionRead}}}); err != nil {
		t.Fatal(err)
	}
	if err := pol.Bind(authz.Binding{Subject: sub, Role: "backup-updater", Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
}

func TestEnablingVerificationNeedsRestorePermissionAndValidSchedule(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	bad := func(v api.RestoreVerifyPolicy) error {
		_, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Schedule: "0 2 * * *", Verify: &v})
		return err
	}
	if err := bad(api.RestoreVerifyPolicy{Enabled: true, Schedule: "nope"}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("bad verify schedule: %v", err)
	}
	if err := bad(api.RestoreVerifyPolicy{Enabled: true, Schedule: "0 4 * * *", MaxMinutes: 241}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("max minutes: %v", err)
	}
	// An operator may set the backup policy (update) only where granted; a subject with
	// update but no restore permission cannot enable verification.
	sub := authz.Subject{ID: "upd", Kind: authz.SubjectUser}
	grantBackupUpdateOnly(t, env, sub)
	_, err := env.b.SetBackupPolicy(ctx, sub, "clu-1", api.BackupPolicy{Schedule: "0 2 * * *"})
	if err != nil {
		t.Fatalf("update-only subject may set a plain policy: %v", err)
	}
	_, err = env.b.SetBackupPolicy(ctx, sub, "clu-1", api.BackupPolicy{Schedule: "0 2 * * *", Verify: &api.RestoreVerifyPolicy{Enabled: true, Schedule: "0 4 * * *"}})
	if !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("enabling verification without restore permission: %v", err)
	}
	// Disabled verification does not need it.
	if _, err := env.b.SetBackupPolicy(ctx, sub, "clu-1", api.BackupPolicy{Schedule: "0 2 * * *", Verify: &api.RestoreVerifyPolicy{Enabled: false, Schedule: "0 4 * * *"}}); err != nil {
		t.Fatalf("disabled verification: %v", err)
	}
}
