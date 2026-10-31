// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
)

func enableBinlog(t *testing.T, env *backupEnv) {
	t.Helper()
	if _, _, err := Update(env.svc.Store(), CollClusters, "clu-1", func(cur ClusterRecord, _ bool) (ClusterRecord, error) {
		cur.Spec.Binlog = true
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchivePolicyValidation(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	pol := api.BackupPolicy{Archive: &api.ArchivePolicy{Enabled: true, Schedule: "0 * * * *"}}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", pol); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("cluster without binlog: %v", err)
	}
	enableBinlog(t, env)
	pol.Archive.Schedule = "nope"
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", pol); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("bad schedule: %v", err)
	}
	pol.Archive.Schedule = "0 * * * *"
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", pol); err != nil {
		t.Fatalf("valid archive policy: %v", err)
	}
	// A disabled archive policy needs neither binlog nor a valid schedule.
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Archive: &api.ArchivePolicy{Schedule: "nope"}}); err != nil {
		t.Fatalf("disabled archive policy: %v", err)
	}
}

func TestScheduledBinlogArchive(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	if _, err := EnsureSystemBinding(env.ops.Authorizer.Policy()); err != nil {
		t.Fatal(err)
	}
	enableBinlog(t, env)
	start := time.Date(2026, 10, 22, 10, 30, 0, 0, time.UTC)
	env.clock.Set(start)
	env.binlogBackup(t, "b1", start.Add(-time.Hour), 0)
	pol := api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *", Archive: &api.ArchivePolicy{Enabled: true, Schedule: "0 * * * *"}}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", pol); err != nil {
		t.Fatal(err)
	}

	// Not due before the next full hour: nothing is started.
	env.b.RunArchives(ctx, collect.SystemSubject)
	if busy, _ := env.ops.unfinishedFor("clu-1"); busy != "" {
		t.Fatalf("started before the schedule was due: %s", busy)
	}

	env.clock.Set(time.Date(2026, 10, 22, 11, 1, 0, 0, time.UTC))
	env.b.RunArchives(ctx, collect.SystemSubject)
	busy, _ := env.ops.unfinishedFor("clu-1")
	if busy == "" {
		t.Fatal("the due archive run did not start")
	}
	rec, _, _ := Get[BackupPolicyRecord](env.svc.Store(), CollBackupPolicies, "clu-1", Consistent())
	if want := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC); !rec.Value.ArchiveLast.Equal(want) {
		t.Fatalf("ArchiveLast %v, want %v", rec.Value.ArchiveLast, want)
	}
	// The backup scheduler rewrites the record; it must keep ArchiveLast.
	env.clock.Set(time.Date(2026, 10, 23, 2, 1, 0, 0, time.UTC))
	env.b.RunSchedules(collect.SystemSubject)
	rec, _, _ = Get[BackupPolicyRecord](env.svc.Store(), CollBackupPolicies, "clu-1", Consistent())
	if rec.Value.ArchiveLast.IsZero() {
		t.Fatal("RunSchedules dropped ArchiveLast")
	}
}
