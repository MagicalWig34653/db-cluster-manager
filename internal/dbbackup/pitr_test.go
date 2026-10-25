// SPDX-License-Identifier: AGPL-3.0-only
package dbbackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

type fakePITR struct {
	calls []string
	deny  bool
}

func (f *fakePITR) CheckRestoreTarget(_ context.Context, a agent.CheckRestoreTargetArgs) error {
	f.calls = append(f.calls, "check")
	if f.deny {
		return ErrProductionTarget
	}
	if a.Expected.ClusterID != InspectionPrefix+"s1" {
		return ErrProductionTarget
	}
	return nil
}
func (f *fakePITR) InspectionRestore(_ context.Context, r InspectionResult) error {
	f.calls = append(f.calls, "inspect")
	return nil
}
func (f *fakePITR) Replay(_ context.Context, m Manifest, start BinlogPosition, target PITRTarget) error {
	f.calls = append(f.calls, fmt.Sprintf("replay:%s:%d", m.BackupID, start.Offset))
	return nil
}

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
		m, err := ArchiveBinlog(context.Background(), be, pub, req.Source.Manifest, fmt.Sprintf("log-%d", i), r, bytes.NewReader(bytes.Repeat([]byte{byte(i)}, 128)), t0.Add(3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		logs = append(logs, m)
	}
	return req, logs, be, key
}
func TestPITRPlanAndFakeExecution(t *testing.T) {
	req, logs, be, key := pitrFixture(t)
	ctx := context.Background()
	target := t0.Add(90 * time.Minute)
	plan, err := PlanPITR(req, prodInventory(), logs, PITRTarget{Time: &target})
	if err != nil {
		t.Fatal(err)
	}
	requireAgentAccepts(t, plan.Inspection.Operation, req.VolumeBase)
	requireApprovalOnDestructive(t, plan.Inspection.Operation, req.ApprovalID)
	if len(plan.Segments) != 2 {
		t.Fatal("missing replay segment")
	}
	f := &fakePITR{}
	if err := ExecutePITRFake(ctx, plan, "", be, key, f); !errors.Is(err, ErrApprovalRequired) {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatal("effect before approval")
	}
	if err := ExecutePITRFake(ctx, plan, plan.Digest, be, key, f); err != nil {
		t.Fatal(err)
	}
	want := "[check inspect replay:log-1:64 replay:log-2:4]"
	if fmt.Sprint(f.calls) != want {
		t.Fatalf("%v", f.calls)
	}
	pos := BinlogPosition{File: "mariadb-bin.000001", Offset: 100}
	plan, err = PlanPITR(req, prodInventory(), logs, PITRTarget{Position: &pos})
	if err != nil || len(plan.Segments) != 1 {
		t.Fatalf("position: %v", err)
	}
	deny := &fakePITR{deny: true}
	if err := ExecutePITRFake(ctx, plan, plan.Digest, be, key, deny); !errors.Is(err, ErrProductionTarget) || len(deny.calls) != 1 {
		t.Fatalf("target guard: %v %v", err, deny.calls)
	}
	plan.BasePosition.Offset++
	if err := ExecutePITRFake(ctx, plan, plan.Digest, be, key, &fakePITR{}); !errors.Is(err, ErrApprovalRequired) {
		t.Fatal("changed plan accepted")
	}
}
func TestPITRRejectsGapsAndTampering(t *testing.T) {
	req, logs, be, key := pitrFixture(t)
	target := t0.Add(90 * time.Minute)
	cases := map[string]func(*InspectionRequest, []Manifest){
		"gap":     func(_ *InspectionRequest, m []Manifest) { m[1].Binlog.Sequence = 3 },
		"history": func(_ *InspectionRequest, m []Manifest) { m[1].Binlog.HistoryID = "other" },
		"source":  func(_ *InspectionRequest, m []Manifest) { m[1].SourceNodeUUID = uuid3 },
		"base":    func(_ *InspectionRequest, m []Manifest) { m[1].Binlog.BaseBackupID = "other" },
		"file": func(_ *InspectionRequest, m []Manifest) {
			m[1].Binlog.Start.File = "mariadb-bin.000003"
			m[1].Binlog.End.File = m[1].Binlog.Start.File
		},
		"time gap": func(_ *InspectionRequest, m []Manifest) { m[1].Binlog.From = m[1].Binlog.From.Add(time.Second) },
		"base offset tamper": func(r *InspectionRequest, _ []Manifest) {
			p := *r.Source.Manifest.BinlogBase
			p.Offset++
			r.Source.Manifest.BinlogBase = &p
		},
		"no base":    func(r *InspectionRequest, _ []Manifest) { r.Source.Manifest.BinlogBase = nil },
		"production": func(r *InspectionRequest, _ []Manifest) { r.Network = "dbcm-prod-net" },
		"approval":   func(r *InspectionRequest, _ []Manifest) { r.ApprovalID = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := req
			m := append([]Manifest(nil), logs...)
			for i := range m {
				b := *m[i].Binlog
				m[i].Binlog = &b
			}
			mut(&r, m)
			if _, err := PlanPITR(r, prodInventory(), m, PITRTarget{Time: &target}); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
	if err := VerifyBinlog(context.Background(), be, logs[0], key); err != nil {
		t.Fatal(err)
	}
	logs[0].Binlog.HistoryID = "tampered"
	if err := VerifyBinlog(context.Background(), be, logs[0], key); err == nil {
		t.Fatal("range tampering accepted")
	}
	logs[0].Binlog.HistoryID = "history-1"
	raw, _ := be.Get(context.Background(), logs[0].Object)
	data := new(bytes.Buffer)
	_, _ = data.ReadFrom(raw)
	_ = raw.Close()
	b := data.Bytes()
	b[len(b)-1] ^= 1
	if err := be.Put(context.Background(), logs[0].Object, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBinlog(context.Background(), be, logs[0], key); err == nil {
		t.Fatal("corrupt stream accepted")
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
