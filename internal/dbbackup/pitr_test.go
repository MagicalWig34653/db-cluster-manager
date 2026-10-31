// SPDX-License-Identifier: AGPL-3.0-only
package dbbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// pitrFixture returns an inspection request for a backup with a binlog
// position and two archived closed files covering t0..t0+2h.
func pitrFixture(t *testing.T) (InspectionRequest, []Manifest, *backup.MemBackend, backup.RecoveryKey) {
	t.Helper()
	req := inspectionRequest(t.TempDir())
	req.Source.Manifest.BinlogBase = &BinlogPosition{File: "mariadb-bin.000001", Offset: 64}
	be := backup.NewMemBackend()
	key := testKey(t)
	pub, _ := key.StreamPublicKey()
	var logs []Manifest
	for i := 1; i <= 2; i++ {
		r := BinlogRange{BaseBackupID: req.Source.Manifest.BackupID, HistoryID: "history-1", Sequence: uint64(i), Start: BinlogPosition{File: fmt.Sprintf("mariadb-bin.%06d", i), Offset: 4}, End: BinlogPosition{File: fmt.Sprintf("mariadb-bin.%06d", i), Offset: 128}, From: t0.Add(time.Duration(i-1) * time.Hour), Until: t0.Add(time.Duration(i) * time.Hour)}
		m, err := ArchiveBinlog(context.Background(), be, pub, req.Source.Manifest, BinlogArchiveID(req.Source.Manifest.BackupID, uint64(i)), r, bytes.NewReader(bytes.Repeat([]byte{byte(i)}, 128)), t0.Add(3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		logs = append(logs, m)
	}
	return req, logs, be, key
}

func clone(m []Manifest) []Manifest {
	out := slices.Clone(m)
	for i := range out {
		b := *out[i].Binlog
		out[i].Binlog = &b
	}
	return out
}

func TestResolvePITR(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	base := req.Source.Manifest
	seg := func(at time.Time) (int, error) {
		s, err := ResolvePITR(base, logs, at)
		return len(s), err
	}
	if n, err := seg(t0.Add(30 * time.Minute)); err != nil || n != 1 {
		t.Fatalf("inside the first file: %d %v", n, err)
	}
	if n, err := seg(t0.Add(90 * time.Minute)); err != nil || n != 2 {
		t.Fatalf("inside the second file: %d %v", n, err)
	}
	// A time equal to a file's last event may continue in the next file.
	if n, err := seg(t0.Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("at the boundary: %d %v", n, err)
	}
	cases := map[string]struct {
		at   time.Time
		want string
	}{
		"before the backup": {base.Finished.Add(-time.Second), "before backup"},
		"after the archive": {t0.Add(2 * time.Hour), "later changes are not archived yet"},
		"far after":         {t0.Add(48 * time.Hour), "later changes are not archived yet"},
		"zero":              {time.Time{}, "target time required"},
	}
	for name, c := range cases {
		_, err := ResolvePITR(base, logs, c.at)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if name != "zero" && !errors.Is(err, ErrPITRNotCovered) {
			t.Fatalf("%s: %v is not ErrPITRNotCovered", name, err)
		}
	}
}

func TestResolvePITRNamesTheGap(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	base := req.Source.Manifest
	at := t0.Add(90 * time.Minute)
	cases := map[string]struct {
		mut  func([]Manifest)
		want string
	}{
		"sequence 2 missing": {func(m []Manifest) { m[1].Binlog.Sequence = 3 }, "sequence 2 is missing after 1"},
		"history changed":    {func(m []Manifest) { m[1].Binlog.HistoryID = "other" }, "history changed"},
		"file skipped": {func(m []Manifest) {
			m[1].Binlog.Start.File = "mariadb-bin.000003"
			m[1].Binlog.End.File = m[1].Binlog.Start.File
		}, "does not follow"},
		"time gap":      {func(m []Manifest) { m[1].Binlog.From = m[1].Binlog.From.Add(time.Second) }, "not continuous"},
		"first missing": {func(m []Manifest) { m[0], m[1] = m[1], m[0]; m[0].Binlog.Sequence = 2; m[1].Binlog.Sequence = 3 }, "sequence 1"},
		"wrong file": {func(m []Manifest) {
			m[0].Binlog.Start.File = "mariadb-bin.000009"
			m[0].Binlog.End.File = "mariadb-bin.000009"
		}, "does not contain the backup position"},
	}
	for name, c := range cases {
		m := clone(logs)
		c.mut(m)
		_, err := ResolvePITR(base, m, at)
		if err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, ErrPITRNotCovered) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Archives that are not this backup's history are an error, not a gap.
	for name, mut := range map[string]func([]Manifest){
		"source":   func(m []Manifest) { m[1].SourceNodeUUID = uuid3 },
		"base":     func(m []Manifest) { m[1].Binlog.BaseBackupID = "other" },
		"position": func(m []Manifest) { m[1].Binlog.BasePosition.Offset++ },
	} {
		m := clone(logs)
		mut(m)
		if _, err := ResolvePITR(base, m, at); err == nil || errors.Is(err, ErrPITRNotCovered) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	noBinlog := base
	noBinlog.BinlogBase = nil
	if _, err := ResolvePITR(noBinlog, logs, at); err == nil || !strings.Contains(err.Error(), "binary log was off") {
		t.Fatal(err)
	}
	if _, err := ResolvePITR(base, nil, at); err == nil || !strings.Contains(err.Error(), "no closed binlog file is archived") {
		t.Fatal(err)
	}
}

func TestCoverageOf(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	base := req.Source.Manifest
	c, err := CoverageOf(base, logs)
	if err != nil || c.Segments != 2 || !c.From.Equal(base.Finished) || !c.Until.Equal(t0.Add(2*time.Hour)) || c.Gap != "" {
		t.Fatalf("%+v %v", c, err)
	}
	m := clone(logs)
	m[1].Binlog.Sequence = 3
	c, err = CoverageOf(base, m)
	if err != nil || c.Segments != 1 || !c.Until.Equal(t0.Add(time.Hour)) || !strings.Contains(c.Gap, "sequence 2 is missing") {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = CoverageOf(base, nil)
	if err != nil || c.Segments != 0 || !c.Until.IsZero() {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestListBinlogArchives(t *testing.T) {
	req, logs, be, _ := pitrFixture(t)
	ctx := context.Background()
	got, err := ListBinlogArchives(ctx, be, testCluster, req.Source.Manifest.BackupID)
	if err != nil || len(got) != 2 || got[0].Binlog.Sequence != 1 || got[1].Binlog.Sequence != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := ListBinlogArchives(ctx, be, testCluster, "other"); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	// A stream without its manifest is an incomplete archive and is ignored.
	_ = be.Delete(ctx, ManifestName(logs[1].Object))
	if got, _ := ListBinlogArchives(ctx, be, testCluster, req.Source.Manifest.BackupID); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestInspectionPlanWithPITR(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	req.PITR = &PITRRequest{At: t0.Add(90 * time.Minute), Archives: logs, ReplaySecret: ReplaySecretName("s1")}
	res, err := InspectionRestorePlan(req, prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	requireAgentAccepts(t, res.Operation, req.VolumeBase)
	requireApprovalOnDestructive(t, res.Operation, req.ApprovalID)
	var ids []string
	for _, s := range res.Operation.Steps {
		ids = append(ids, s.ID)
	}
	want := []string{"create-instance", "replay", "wait-replay", "remove-replay-job", "remove-replay-secret", "verify"}
	if tail := ids[len(ids)-len(want):]; !slices.EqualFunc(tail, want, func(a, b string) bool { return strings.HasSuffix(a, "-"+b) }) {
		t.Fatalf("steps %v", ids)
	}
	var replay agent.RunRestoreArgs
	for _, s := range res.Operation.Steps {
		if s.Command == agent.KindRunRestore && strings.HasSuffix(s.ID, "-replay") {
			if err := json.Unmarshal(s.Args, &replay); err != nil {
				t.Fatal(err)
			}
		}
	}
	if replay.Mode != agent.RestoreBinlog || replay.DBHost != "dbcm-inspect-s1" || replay.Network != req.Network || replay.DBCredentialSecret != "dbcm-inspect-s1-replay" ||
		replay.StartOffset != 64 || replay.StopDatetime != "2026-10-22 02:30:01" || len(replay.Segments) != 2 || replay.ApprovalID != req.ApprovalID {
		t.Fatalf("%+v", replay)
	}
	// The same plan without PITR has no replay steps.
	req.PITR = nil
	plain, err := InspectionRestorePlan(req, prodInventory())
	if err != nil || len(plain.Operation.Steps) != len(res.Operation.Steps)-4 {
		t.Fatalf("%d %v", len(plain.Operation.Steps), err)
	}
	// An uncovered target never produces a plan.
	req.PITR = &PITRRequest{At: t0.Add(5 * time.Hour), Archives: logs, ReplaySecret: ReplaySecretName("s1")}
	if _, err := InspectionRestorePlan(req, prodInventory()); !errors.Is(err, ErrPITRNotCovered) {
		t.Fatal(err)
	}
	req.PITR = &PITRRequest{At: t0.Add(time.Minute), Archives: logs}
	if _, err := InspectionRestorePlan(req, prodInventory()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no replay secret: %v", err)
	}
}

func TestPITRRestoreNeverTargetsProduction(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	req.PITR = &PITRRequest{At: t0.Add(time.Minute), Archives: logs, ReplaySecret: ReplaySecretName("s1")}
	for name, mut := range map[string]func(*InspectionRequest){
		"production network": func(r *InspectionRequest) { r.Network = "dbcm-prod-net" },
		"no approval":        func(r *InspectionRequest) { r.ApprovalID = "" },
	} {
		r := req
		mut(&r)
		if _, err := InspectionRestorePlan(r, prodInventory()); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestReplaySecretAndInit(t *testing.T) {
	pw, _ := NewInspectionPassword()
	rp, _ := NewInspectionPassword()
	sql, err := RenderInspectionInitReplay(pw, rp, LocalAccount{User: "monitor", Host: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE USER IF NOT EXISTS 'dbcm_replay'@'%' IDENTIFIED BY '" + rp + "'", "GRANT ALL PRIVILEGES ON *.* TO 'dbcm_replay'@'%'",
		"(User = 'dbcm_replay' AND Host = '%')", "account_locked"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %q in\n%s", want, sql)
		}
	}
	// A plain inspection never creates the replay account.
	plain, _ := RenderInspectionInit(pw)
	if strings.Contains(plain, ReplayAccount) {
		t.Fatal("plain inspection init mentions the replay account")
	}
	if _, err := RenderInspectionInitReplay(pw, "not hex"); err == nil {
		t.Fatal("bad replay password accepted")
	}
	opt, err := RenderReplayOptions(rp, 3307)
	if err != nil || opt != "[client]\nuser=dbcm_replay\npassword="+rp+"\nport=3307\n" {
		t.Fatalf("%q %v", opt, err)
	}
	if _, err := RenderReplayOptions("x", 3307); err == nil {
		t.Fatal("bad password accepted")
	}
}

func TestTeardownRemovesReplayLeftovers(t *testing.T) {
	req, logs, _, _ := pitrFixture(t)
	req.PITR = &PITRRequest{At: t0.Add(time.Minute), Archives: logs, ReplaySecret: ReplaySecretName("s1")}
	res, err := InspectionRestorePlan(req, prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	td, err := TeardownPlanFor(TeardownRequest{OpID: "op-t", InstallationID: testInst, Session: res.Session, InitSecret: InitSecretName("s1"), ReplaySecret: ReplaySecretName("s1")}, prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range td.Steps {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, " "); !strings.Contains(got, "remove-replay-job") || !strings.Contains(got, "remove-replay-secret") {
		t.Fatalf("%s", got)
	}
	if _, err := TeardownPlanFor(TeardownRequest{OpID: "op-t", InstallationID: testInst, Session: res.Session, ReplaySecret: "prod-root-password"}, prodInventory()); !errors.Is(err, ErrProductionTarget) {
		t.Fatalf("foreign replay secret: %v", err)
	}
	requireAgentAccepts(t, td, req.VolumeBase)
}

func TestBinlogTamperingIsRejected(t *testing.T) {
	_, logs, be, key := pitrFixture(t)
	ctx := context.Background()
	if err := VerifyBinlog(ctx, be, logs[0], key); err != nil {
		t.Fatal(err)
	}
	logs[0].Binlog.HistoryID = "tampered"
	if err := VerifyBinlog(ctx, be, logs[0], key); err == nil {
		t.Fatal("range tampering accepted")
	}
	logs[0].Binlog.HistoryID = "history-1"
	raw, _ := be.Get(ctx, logs[0].Object)
	data := new(bytes.Buffer)
	_, _ = data.ReadFrom(raw)
	_ = raw.Close()
	b := data.Bytes()
	b[len(b)-1] ^= 1
	if err := be.Put(ctx, logs[0].Object, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBinlog(ctx, be, logs[0], key); err == nil {
		t.Fatal("corrupt stream accepted")
	}
}

func TestBinlogArchivePlan(t *testing.T) {
	req, _, _, _ := pitrFixture(t)
	base := req.Source.Manifest
	base.SourceNodeUUID = uuid2
	ar := BinlogArchiveRequest{OpID: "op-a1", Base: base, InstallationID: testInst, ClusterName: "prod", JobImage: "dbcm/job:1", Network: "dbcm-prod-net",
		DBCredentialSecret: "db-cred", BackendSecret: "backend-cfg", RecipientKey: pubString(t, testKey(t)), VolumeBase: t.TempDir(),
		Job: agent.JobResources{NanoCPUs: 1e9, MemoryBytes: gib}, ClusterSize: 3, ServiceHost: "m1"}
	op, srcNode, err := BinlogArchivePlan(ar, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	if srcNode.Slot != "n2" {
		t.Fatalf("source %+v", srcNode)
	}
	requireAgentAccepts(t, op, ar.VolumeBase)
	var run agent.RunBackupArgs
	for _, s := range op.Steps {
		if s.Command == agent.KindRunBackup {
			if err := json.Unmarshal(s.Args, &run); err != nil {
				t.Fatal(err)
			}
			if s.Target != "m1" {
				t.Fatalf("job runs on %s, want the service host", s.Target)
			}
		}
	}
	if run.Tool != agent.ToolBinlogArchive || run.NodeID != "h2" || run.SourceVolume != "dbcm-prod-n2-00000000" || run.DBHost != "dbcm-prod-n2" ||
		run.BackupID != base.BackupID || run.Object != base.Object || run.Expected.NodeUUID != uuid2 {
		t.Fatalf("%+v", run)
	}
	if !slices.Contains(op.Locks, "cluster/"+base.ClusterID) {
		t.Fatalf("locks %v", op.Locks)
	}
	// The source node was replaced: the chain cannot continue.
	nodes := threeNodes()
	nodes[1].NodeUUID = "00000000-0000-4000-8000-0000000000ff"
	if _, _, err := BinlogArchivePlan(ar, nodes); !errors.Is(err, ErrNoSource) {
		t.Fatalf("%v", err)
	}
	ar.Base.BinlogBase = nil
	if _, _, err := BinlogArchivePlan(ar, threeNodes()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("%v", err)
	}
}

func TestBinlogRetentionFollowsFullBackup(t *testing.T) {
	sets := []BackupSet{{BackupID: "old", Created: t0}, {BackupID: "new", Created: t0.Add(time.Hour)}, {BackupID: "log", BaseBackupID: "old", Created: t0.Add(2 * time.Hour)}}
	keep, remove := RetentionSelect(sets, backup.Policy{KeepLast: 1})
	if len(keep) != 1 || len(remove) != 2 || remove[0].BackupID != "log" {
		t.Fatalf("keep %v remove %v", keep, remove)
	}
	sets[2].Pinned = true
	keep, remove = RetentionSelect(sets, backup.Policy{KeepLast: 1})
	if len(keep) != 3 || len(remove) != 0 {
		t.Fatalf("pin failed %v %v", keep, remove)
	}
}
