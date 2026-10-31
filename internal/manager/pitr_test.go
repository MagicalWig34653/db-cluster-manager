// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// binlogBackup stores a completed backup b1 that recorded the binlog
// position and archives n closed files after it: file i covers
// [ended+(i-1)*30m, ended+i*30m).
func (env *backupEnv) binlogBackup(t *testing.T, id string, ended time.Time, n int) {
	t.Helper()
	env.completedBackup(t, id, ended)
	ctx := context.Background()
	obj := dbbackup.ObjectName("clu-1", id, dbbackup.MethodPhysical, "")
	m, err := (&Backups{Backend: env.be}).manifest(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	m.BinlogBase = &dbbackup.BinlogPosition{File: "dbcm-bin.000001", Offset: 64}
	raw, _ := json.Marshal(m)
	if err := env.be.Put(ctx, dbbackup.ManifestName(obj), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	key, err := backup.GenerateRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := key.StreamPublicKey()
	for i := 1; i <= n; i++ {
		file := fmt.Sprintf("dbcm-bin.%06d", i)
		r := dbbackup.BinlogRange{BaseBackupID: id, HistoryID: m.SourceNodeUUID, Sequence: uint64(i),
			Start: dbbackup.BinlogPosition{File: file, Offset: 4}, End: dbbackup.BinlogPosition{File: file, Offset: 128},
			From: ended.Add(time.Duration(i-1) * 30 * time.Minute), Until: ended.Add(time.Duration(i) * 30 * time.Minute)}
		if _, err := dbbackup.ArchiveBinlog(ctx, env.be, pub, m, dbbackup.BinlogArchiveID(id, uint64(i)), r, bytes.NewReader(make([]byte, 128)), ended); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProposePointInTime(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	ended := env.clock.Now().Add(-3 * time.Hour)
	if _, err := env.b.ProposePointInTime(ctx, nobody, "clu-1", ended); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := env.b.ProposePointInTime(ctx, adminSubject, "nope", ended); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	// No backup at all.
	p, err := env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended)
	if err != nil || p.Covered || !strings.Contains(p.Reason, "no completed backup ended before") || !p.ApprovalRequired || p.InPlace {
		t.Fatalf("%+v %v", p, err)
	}
	// A backup of a cluster whose binary log was off.
	env.completedBackup(t, "b0", ended.Add(-24*time.Hour))
	p, _ = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended)
	if p.Covered || p.Backup != "b0" || !strings.Contains(p.Reason, "binary log was off") || p.Coverage != nil {
		t.Fatalf("%+v", p)
	}
	// With two archived files.
	env.binlogBackup(t, "b1", ended, 2)
	p, err = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended.Add(20*time.Minute))
	if err != nil || !p.Covered || p.Backup != "b1" || p.Segments != 1 || p.Reason != "" || p.Coverage == nil || p.Coverage.Segments != 2 || !p.Coverage.Until.Equal(ended.Add(time.Hour)) {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _ = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended.Add(45*time.Minute)); !p.Covered || p.Segments != 2 {
		t.Fatalf("%+v", p)
	}
	// After the archive: the reason says what is missing and what is covered.
	p, _ = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended.Add(2*time.Hour))
	if p.Covered || p.Backup != "b1" || !strings.Contains(p.Reason, "later changes are not archived yet") || p.Coverage == nil || p.Coverage.Segments != 2 {
		t.Fatalf("%+v", p)
	}
	// A gap in the archive is named.
	if err := env.be.Delete(ctx, dbbackup.ManifestName(dbbackup.ObjectName("clu-1", dbbackup.BinlogArchiveID("b1", 1), dbbackup.MethodBinlog, ""))); err != nil {
		t.Fatal(err)
	}
	p, _ = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended.Add(45*time.Minute))
	if p.Covered || !strings.Contains(p.Reason, "sequence 1") || p.Coverage == nil || p.Coverage.Segments != 0 {
		t.Fatalf("%+v", p)
	}
	// A time before every backup is not covered either.
	if p, _ = env.b.ProposePointInTime(ctx, adminSubject, "clu-1", ended.Add(-72*time.Hour)); p.Covered || !strings.Contains(p.Reason, "no completed backup ended before") {
		t.Fatalf("%+v", p)
	}
}

func TestInspectionToAPointInTimeNeedsApprovalAndCoverage(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	ended := env.clock.Now().Add(-3 * time.Hour)
	env.binlogBackup(t, "b1", ended, 2)
	at := ended.Add(40 * time.Minute)
	late := ended.Add(5 * time.Hour)
	if _, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", PointInTime: &late}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "not archived yet") {
		t.Fatalf("uncovered time: %v", err)
	}
	if _, err := env.b.StartInspection(ctx, nobody, api.InspectionInput{Cluster: "clu-1", Host: "h4", PointInTime: &at}); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	res, err := env.b.StartInspection(ctx, adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", PointInTime: &at, TTLMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup != "b1" || res.PointInTime == nil || !res.PointInTime.Equal(at) || len(res.Approval.Digest) != 64 {
		t.Fatalf("%+v", res)
	}
	// The replay account exists only in the init file; the replay secret is the
	// client option file with the same password; neither is stored.
	replay := string(env.conn.secrets["dbcm-inspect-"+res.Session+"-replay"])
	init := string(env.conn.secrets["dbcm-inspect-"+res.Session+"-init"])
	pw := strings.TrimSuffix(strings.SplitN(strings.SplitN(replay, "password=", 2)[1], "\n", 2)[0], "")
	if !strings.HasPrefix(replay, "[client]\nuser=dbcm_replay\n") || len(pw) != 48 || !strings.Contains(init, "IDENTIFIED BY '"+pw+"'") || !strings.Contains(replay, "port=3307") {
		t.Fatalf("replay secret %q", replay)
	}
	for _, d := range mustRaw(t, env.svc, CollOperations, CollInspections) {
		if strings.Contains(string(d), pw) || strings.Contains(string(d), res.InspectionPassword) {
			t.Fatal("a password reached the replicated store")
		}
	}
	rec := opRecord(t, env.svc, res.ID)
	var replayStep *runner.Step
	for i, s := range rec.Definition.Steps {
		if s.Command == agent.KindRunRestore && strings.HasSuffix(s.ID, "-replay") {
			replayStep = &rec.Definition.Steps[i]
		}
	}
	if replayStep == nil {
		t.Fatal("no replay step in the stored definition")
	}
	var a agent.RunRestoreArgs
	if err := json.Unmarshal(replayStep.Args, &a); err != nil {
		t.Fatal(err)
	}
	if a.Mode != agent.RestoreBinlog || len(a.Segments) != 2 || a.StartOffset != 64 || a.StopDatetime != at.UTC().Truncate(time.Second).Add(time.Second).Format("2006-01-02 15:04:05") ||
		!strings.HasPrefix(a.DBHost, "dbcm-inspect-") || a.DBCredentialSecret != "dbcm-inspect-"+res.Session+"-replay" {
		t.Fatalf("%+v", a)
	}
	// Like every inspection: nothing is sent before the digest is approved.
	waitOpStatus(t, env.svc, res.ID, runner.StatusPaused)
	if cmds := env.commandsFor(res.ID); len(cmds) != 0 {
		t.Fatalf("commands before approval: %+v", cmds)
	}
	ctl := &Control{Ops: env.ops, Swarms: env.b.Swarms}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, res.ID, strings.Repeat("0", 64)); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	// The expiry teardown removes the replay leftovers too.
	env.clock.Set(env.clock.Now().Add(31 * time.Minute))
	env.b.TeardownExpired(collect.SystemSubject)
	waitFor(t, "inspection aborted", func() bool { return !opRecord(t, env.svc, res.ID).EndedAt.IsZero() })
	env.b.TeardownExpired(collect.SystemSubject)
	ir, _, _ := Get[InspectionRecord](env.svc.Store(), CollInspections, res.Session)
	if ir.Value.ReplaySecret != "dbcm-inspect-"+res.Session+"-replay" || ir.Value.TeardownOp == "" {
		t.Fatalf("%+v", ir.Value)
	}
	waitOpStatus(t, env.svc, ir.Value.TeardownOp, runner.StatusSucceeded)
	removed := map[string]bool{}
	for _, c := range env.commandsFor(ir.Value.TeardownOp) {
		var s agent.RemoveSecretArgs
		if c.Kind == agent.KindRemoveSecret && json.Unmarshal(c.Args, &s) == nil {
			removed[s.Secret] = true
		}
	}
	if !removed["dbcm-inspect-"+res.Session+"-replay"] || !removed["dbcm-inspect-"+res.Session+"-init"] || len(removed) != 2 {
		t.Fatalf("removed secrets %v", removed)
	}
}

func TestStartBinlogArchive(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	if _, err := env.b.StartBinlogArchive(ctx, nobody, "clu-1"); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := env.b.StartBinlogArchive(ctx, adminSubject, "clu-1"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "binlog position") {
		t.Fatalf("no backup: %v", err)
	}
	ended := env.clock.Now().Add(-time.Hour)
	env.completedBackup(t, "b0", ended.Add(-time.Hour)) // log off: skipped
	env.binlogBackup(t, "b1", ended, 0)
	ref, err := env.b.StartBinlogArchive(ctx, adminSubject, "clu-1")
	if err != nil || ref.Kind != dbbackup.OpBinlogArchive {
		t.Fatalf("%+v %v", ref, err)
	}
	rec := opRecord(t, env.svc, ref.ID)
	var run agent.RunBackupArgs
	for _, s := range rec.Definition.Steps {
		if s.Command == agent.KindRunBackup {
			if err := json.Unmarshal(s.Args, &run); err != nil {
				t.Fatal(err)
			}
		}
	}
	if run.Tool != agent.ToolBinlogArchive || run.BackupID != "b1" || run.Expected.NodeUUID != "00000000-0000-4000-8000-000000000002" || run.DBCredentialSecret != "dbcm-backup-cred" {
		t.Fatalf("%+v", run)
	}
	waitFor(t, "archive job requested", func() bool {
		for _, c := range env.commandsFor(ref.ID) {
			if c.Kind == agent.KindRunBackup {
				return true
			}
		}
		return false
	})
	// While it is unfinished, neither a second archive nor a backup starts.
	if _, err := env.b.StartBinlogArchive(ctx, adminSubject, "clu-1"); !errors.Is(err, api.ErrBusy) {
		t.Fatalf("second archive: %v", err)
	}
}

// TestRetentionRemovesBinlogArchivesWithTheirBackup: the archive of a removed
// full backup goes first, the archive of a kept one stays.
func TestRetentionRemovesBinlogArchivesWithTheirBackup(t *testing.T) {
	env := newBackupEnv(t, true)
	ctx := context.Background()
	old := env.clock.Now().Add(-48 * time.Hour)
	env.binlogBackup(t, "b1", old, 2)
	env.binlogBackup(t, "b2", old.Add(24*time.Hour), 1)
	env.binlogBackup(t, "b3", old.Add(40*time.Hour), 1)
	if _, err := env.b.SetBackupPolicy(ctx, adminSubject, "clu-1", api.BackupPolicy{Enabled: false, Schedule: "0 2 * * *", Retention: &api.BackupRetention{KeepLast: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := env.b.ApplyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	left := func(id string) int {
		objs, _ := env.be.List(ctx, dbbackup.BinlogArchivePrefix("clu-1", id))
		return len(objs)
	}
	if left("b1") != 0 || left("b2") != 0 {
		t.Fatalf("archives of removed backups left: b1 %d, b2 %d", left("b1"), left("b2"))
	}
	if left("b3") == 0 {
		t.Fatal("the archive of the kept backup was removed")
	}
	if _, deleted := liveBackups(t, env); len(deleted) != 2 {
		t.Fatalf("deleted %d backups", len(deleted))
	}
}
