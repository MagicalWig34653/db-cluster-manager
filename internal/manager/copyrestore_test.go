// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// TestInspectionOfCopyUsesTargetSecretAndBackend: a copy is restored into an
// isolated inspection instance through the target's own Swarm secret; the
// operation names the target so that the result gates read the job status
// there. Unverifiable, foreign, PITR or secret-less requests are refused.
func TestInspectionOfCopyUsesTargetSecretAndBackend(t *testing.T) {
	env, dr, _ := copyEnv(t)
	ctx := context.Background()
	obj := env.completedBackupWithData(t, "b1", env.clock.Now().Add(-time.Hour))
	cp, err := env.b.StartBackupCopy(ctx, adminSubject, "clu-1", api.BackupCopyInput{Backup: "b1", Target: "dr"})
	if err != nil {
		t.Fatal(err)
	}
	waitCopy(t, env, cp.ID, CopySucceeded)
	in := api.InspectionInput{Cluster: "clu-1", Host: "h4", TTLMinutes: 30, Copy: cp.ID}

	if _, err := env.b.StartInspection(ctx, nobody, in); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	// No Swarm secret for the target: refused, nothing started.
	if _, err := env.b.StartInspection(ctx, adminSubject, in); !errors.Is(err, api.ErrSettingsIncomplete) || !strings.Contains(err.Error(), "targetSecrets") {
		t.Fatalf("without target secret: %v", err)
	}
	if _, _, err := Update(env.svc.Store(), CollSettings, SettingsKey, func(s Settings, _ bool) (Settings, error) {
		s.Backup.TargetSecrets = map[string]string{"dr": "dbcm-backend-dr"}
		return s, nil
	}); err != nil {
		t.Fatal(err)
	}
	at := env.clock.Now()
	for name, bad := range map[string]api.InspectionInput{
		"unknown copy":  {Cluster: "clu-1", Host: "h4", Copy: "cp-nope"},
		"with backup":   {Cluster: "clu-1", Host: "h4", Copy: cp.ID, Backup: "b1"},
		"with PITR":     {Cluster: "clu-1", Host: "h4", Copy: cp.ID, PointInTime: &at},
		"other cluster": {Cluster: "clu-2", Host: "h4", Copy: cp.ID},
	} {
		if _, err := env.b.StartInspection(ctx, adminSubject, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ops, _ := List[OperationRecord](env.svc.Store(), CollOperations); len(ops) != 1 {
		t.Fatalf("a refused request left %d operations (want only the backup's)", len(ops))
	}

	// A tampered copy is refused at the start (deep verification).
	good, err := readBackend(dr, dbbackup.ManifestName(obj))
	if err != nil {
		t.Fatal(err)
	}
	if err := dr.Put(ctx, dbbackup.ManifestName(obj), strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.b.StartInspection(ctx, adminSubject, in); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("tampered copy: %v", err)
	}
	if err := dr.Put(ctx, dbbackup.ManifestName(obj), bytes.NewReader(good)); err != nil {
		t.Fatal(err)
	}

	res, err := env.b.StartInspection(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	rec := opRecord(t, env.svc, res.ID)
	if rec.Backend != "dr" || res.Backup != "b1" {
		t.Fatalf("backend %q backup %q", rec.Backend, res.Backup)
	}
	var run agent.RunRestoreArgs
	for _, st := range rec.Definition.Steps {
		if st.Command == agent.KindRunRestore {
			if err := json.Unmarshal(st.Args, &run); err != nil {
				t.Fatal(err)
			}
		}
	}
	if run.BackendSecret != "dbcm-backend-dr" {
		t.Fatalf("restore job mounts %q, want the target secret", run.BackendSecret)
	}
	if d, _ := DefinitionDigest(rec.Definition); d != res.Approval.Digest {
		t.Fatal("digest does not cover the definition")
	}

	// The result gate reads the job status from the target, not the primary.
	rc := env.svc.cfg.Checker.(*RoutingChecker)
	rc.Backup = &dbbackup.Checker{Backend: env.be}
	rc.Targets = map[string]backup.Backend{"dr": dr}
	ir, _, _ := Get[InspectionRecord](env.svc.Store(), CollInspections, res.Session)
	job := ir.Value.Session.JobName
	st, _ := json.Marshal(dbbackup.JobStatus{Job: job, BackupID: "b1", Succeeded: true, StreamVerified: true})
	check := func() (bool, string) {
		ok, why, err := rc.Check(ctx, res.ID, runner.Step{}, runner.Condition{Kind: dbbackup.CondRestoreComplete, Target: job})
		if err != nil {
			t.Fatal(err)
		}
		return ok, why
	}
	if err := env.be.Put(ctx, dbbackup.StatusName(job), bytes.NewReader(st)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := check(); ok {
		t.Fatal("a status on the primary backend satisfied a copy restore")
	}
	if err := dr.Put(ctx, dbbackup.StatusName(job), bytes.NewReader(st)); err != nil {
		t.Fatal(err)
	}
	if ok, why := check(); !ok {
		t.Fatalf("status on the target not read: %s", why)
	}
	// An unconfigured target is unmet, never the primary backend.
	rc.Targets = nil
	if ok, why := check(); ok || !strings.Contains(why, "not configured") {
		t.Fatalf("unknown target: %v %q", ok, why)
	}
}
