// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// fakeJobs answers run-backup like the job helper: it writes the stream
// object and then the manifest (format 2) into the backend.
func fakeJobs(t *testing.T, env *backupEnv) func(string, agent.Command) (agent.Result, bool) {
	return func(_ string, cmd agent.Command) (agent.Result, bool) {
		if cmd.Kind != agent.KindRunBackup {
			return agent.Result{}, false
		}
		var a agent.RunBackupArgs
		if err := json.Unmarshal(cmd.Args, &a); err != nil {
			t.Error(err)
			return agent.Result{}, false
		}
		ctx := context.Background()
		stream := bytes.Repeat([]byte{7}, 99)
		if err := env.be.Put(ctx, a.Object, bytes.NewReader(stream)); err != nil {
			t.Error(err)
		}
		now := env.clock.Now()
		m := dbbackup.Manifest{Format: dbbackup.ManifestFormat, BackupID: a.BackupID, Method: dbbackup.MethodPhysical, ClusterID: "clu-1",
			SourceNodeUUID: a.Expected.NodeUUID, Object: a.Object, Started: now, Finished: now,
			Stream: backup.StreamSummary{KeyID: "k", PlainSize: 10, PlainMAC: "mm", CipherSize: 99, CipherSHA256: "aa",
				Chunks: []backup.ChunkDigest{{Index: 0, Size: 10, MAC: "bb"}}},
			Stored: backup.PutResult{Size: 99, SHA256: "aa"}, Checkpoints: &dbbackup.Checkpoints{BackupType: "full-backuped", ToLSN: 5}}
		if a.Tool == agent.ToolMariadbDump {
			m.Method, m.Database, m.Checkpoints = dbbackup.MethodLogical, a.Database, nil
		}
		raw, _ := json.Marshal(m)
		if err := env.be.Put(ctx, dbbackup.ManifestName(a.Object), bytes.NewReader(raw)); err != nil {
			t.Error(err)
		}
		return agent.Result{Status: agent.StatusSucceeded}, true
	}
}

func liveBackups(t *testing.T, env *backupEnv) (live, deleted []BackupRecord) {
	t.Helper()
	recs, err := List[BackupRecord](env.svc.Store(), CollBackups)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Value.DeletedAt.IsZero() {
			live = append(live, r.Value)
		} else {
			deleted = append(deleted, r.Value)
		}
	}
	return live, deleted
}

// TestScheduledLogicalBackupsWithRetention runs a daily logical schedule
// over four days against fake job results, then retention with keepLast
// 2: the two oldest sets lose manifest and stream (manifest first), their
// records are marked deleted, and the newest sets stay restorable.
func TestScheduledLogicalBackupsWithRetention(t *testing.T) {
	env := newBackupEnv(t, true)
	rc := env.svc.cfg.Checker.(*RoutingChecker)
	rc.Backup = &dbbackup.Checker{Backend: env.be}
	env.agents.answer = fakeJobs(t, env)
	if _, err := EnsureSystemBinding(env.ops.Authorizer.Policy()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *",
		Method: "logical", Databases: []string{"shop", "crm"}, Retention: &api.BackupRetention{KeepLast: 0}}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("retention without keepLast: %v", err)
	}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *",
		Method: "physical", Databases: []string{"shop"}}); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("physical with databases: %v", err)
	}
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: true, Schedule: "0 2 * * *",
		Method: "logical", Databases: []string{"shop", "crm"}, Retention: &api.BackupRetention{KeepLast: 2}}); err != nil {
		t.Fatal(err)
	}
	for day := 22; day <= 25; day++ {
		env.clock.Set(time.Date(2026, 10, day, 2, 0, 30, 0, time.UTC))
		env.freshHealth(t)
		env.b.RunSchedules(collect.SystemSubject)
		live, _ := liveBackups(t, env)
		var last BackupRecord
		for _, b := range live {
			if b.StartedAt.After(last.StartedAt) {
				last = b
			}
		}
		if last.Method != dbbackup.MethodLogical || len(last.Objects) != 2 || last.Trigger != "schedule" {
			t.Fatalf("day %d: backup %+v", day, last)
		}
		waitOpStatus(t, env.svc, last.OpID, runner.StatusSucceeded)
		// Retention runs only when nothing is running.
		if err := env.b.ApplyRetention(ctx); err != nil {
			t.Fatal(err)
		}
	}
	live, deleted := liveBackups(t, env)
	if len(live) != 2 || len(deleted) != 2 {
		t.Fatalf("live %d deleted %d", len(live), len(deleted))
	}
	for _, d := range deleted {
		for _, obj := range d.Objects {
			if _, err := env.be.Get(ctx, obj); !errors.Is(err, backup.ErrNotFound) {
				t.Fatalf("stream %s of deleted set kept: %v", obj, err)
			}
			if _, err := env.be.Get(ctx, dbbackup.ManifestName(obj)); !errors.Is(err, backup.ErrNotFound) {
				t.Fatalf("manifest %s of deleted set kept: %v", obj, err)
			}
		}
		if d.DeletedAt.IsZero() {
			t.Fatal("deletion not recorded")
		}
	}
	for _, l := range live {
		for _, obj := range l.Objects {
			if _, err := env.be.Get(ctx, dbbackup.ManifestName(obj)); err != nil {
				t.Fatalf("kept set lost %s: %v", obj, err)
			}
		}
	}
	// The summary points at the newest live backup.
	sums, _ := env.b.Backups(ctx)
	if len(sums) != 1 || !slices.ContainsFunc(live, func(b BackupRecord) bool { return b.ID == sums[0].LastBackupID }) {
		t.Fatalf("summary %+v", sums)
	}
}

// TestRetentionKeepsVerifiedAndPinnedSets: the newest verified set and a
// set used by an open inspection survive even when keepLast is 1, and a
// cluster with a running operation is skipped.
func TestRetentionKeepsVerifiedAndPinnedSets(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	for i, id := range []string{"b1", "b2", "b3", "b4"} {
		env.completedBackup(t, id, base.Add(time.Duration(i)*24*time.Hour))
	}
	st := env.svc.Store()
	if _, err := Put(st, CollVerifications, "s-old", dbbackup.VerifyReport{BackupID: "b1", ClusterID: "clu-1", SessionID: "s-old", Passed: true, At: base}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollInspections, "s-open", InspectionRecord{Cluster: "clu-1", Session: dbbackup.InspectionSession{ID: "s-open", BackupID: "b2"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollBackupPolicies, "clu-1", BackupPolicyRecord{Cluster: "clu-1",
		Policy: api.BackupPolicy{Schedule: "0 2 * * *", Retention: &api.BackupRetention{KeepLast: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollOperations, "op-run", OperationRecord{ID: "op-run", Cluster: "clu-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.b.ApplyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if _, deleted := liveBackups(t, env); len(deleted) != 0 {
		t.Fatal("retention ran while an operation was unfinished")
	}
	if _, err := Put(st, CollOperations, "op-run", OperationRecord{ID: "op-run", Cluster: "clu-1", EndedAt: base}, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.b.ApplyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	live, deleted := liveBackups(t, env)
	ids := func(bs []BackupRecord) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(live); !slices.Equal(got, []string{"b1", "b2", "b4"}) || !slices.Equal(ids(deleted), []string{"b3"}) {
		t.Fatalf("live %v deleted %v", got, ids(deleted))
	}
}
