// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func verifyManifest() Manifest {
	m := validManifest()
	sum := uint64(42)
	m.Catalog = &Catalog{Schemas: []Schema{
		{Name: "shop", Tables: []Table{{Name: "orders", Checksum: &sum}, {Name: "customers"}}},
		{Name: "crm", Tables: []Table{{Name: "leads"}}},
	}}
	m.Marker = "b1"
	return m
}

func goodObservation(m Manifest) Observation {
	sample := SampleTables(m.Catalog, m.BackupID, 3)
	cs := map[string]uint64{}
	for _, n := range sample {
		cs[n] = 7
		if n == "shop.orders" {
			cs[n] = 42
		}
	}
	return Observation{StreamVerified: true, ServerReady: true, WsrepOff: true,
		TableCounts: map[string]int{"shop": 2, "crm": 1}, Checksums: cs, Marker: "b1"}
}

func TestEvaluateVerification(t *testing.T) {
	m := verifyManifest()
	r := EvaluateVerification(m, "s1", goodObservation(m), t0)
	if !r.Passed || len(r.Checks) != 5 {
		t.Fatalf("%+v", r)
	}
	fails := map[string]func(*Observation){
		"stream":   func(o *Observation) { o.StreamVerified = false },
		"wsrep":    func(o *Observation) { o.WsrepOff = false },
		"counts":   func(o *Observation) { o.TableCounts["crm"] = 0 },
		"checksum": func(o *Observation) { o.Checksums["shop.orders"] = 41 },
		"marker":   func(o *Observation) { o.Marker = "b0" },
	}
	for name, mut := range fails {
		o := goodObservation(m)
		mut(&o)
		if r := EvaluateVerification(m, "s1", o, t0); r.Passed {
			t.Fatalf("%s: passed", name)
		}
	}
	noCat := validManifest()
	if r := EvaluateVerification(noCat, "s1", goodObservation(m), t0); r.Passed {
		t.Fatal("verification without catalog passed")
	}
	if a, b := SampleTables(m.Catalog, "b1", 2), SampleTables(m.Catalog, "b1", 2); !slices.Equal(a, b) {
		t.Fatal("sample not deterministic")
	}
}

func TestLastVerifiedRecord(t *testing.T) {
	s := &MemVerifiedStore{}
	if _, ok := s.LastVerified(testCluster); ok {
		t.Fatal("empty store")
	}
	m := verifyManifest()
	pass := EvaluateVerification(m, "s1", goodObservation(m), t0)
	o := goodObservation(m)
	o.StreamVerified = false
	fail := EvaluateVerification(m, "s2", o, t0.Add(time.Hour))
	_ = s.Record(pass)
	_ = s.Record(fail)
	got, ok := s.LastVerified(testCluster)
	if !ok || !got.At.Equal(t0) || got.SessionID != "s1" {
		t.Fatalf("last verified %+v: a failed later run must not replace it", got)
	}
	if !s.Verified("b1") || s.Verified("b2") {
		t.Fatal("Verified")
	}
	if err := s.Record(VerifyReport{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func set(id string, created time.Time) BackupSet {
	return BackupSet{BackupID: id, ClusterID: testCluster, Created: created, Objects: []string{"o-" + id}}
}

func ids(s []BackupSet) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.BackupID)
	}
	return out
}

func TestRetentionProtectsVerifiedPinnedAndNewest(t *testing.T) {
	var sets []BackupSet
	for i := range 6 {
		sets = append(sets, set(string(rune('a'+i)), t0.Add(time.Duration(i)*time.Hour)))
	}
	sets[1].Verified = true // "b": the newest verified one
	sets[0].Pinned = true   // "a": an inspection session uses it
	keep, remove := RetentionSelect(sets, backup.Policy{KeepLast: 2})
	if !slices.Equal(ids(keep), []string{"f", "e", "b", "a"}) || !slices.Equal(ids(remove), []string{"d", "c"}) {
		t.Fatalf("keep %v remove %v", ids(keep), ids(remove))
	}
	keep, remove = RetentionSelect(sets, backup.Policy{})
	if len(keep) != 6 || len(remove) != 0 {
		t.Fatal("zero policy must keep everything")
	}
}

func TestApplyRetentionDeletesManifestAndStream(t *testing.T) {
	ctx := context.Background()
	b := backup.NewMemBackend()
	for _, n := range []string{"o-x", ManifestName("o-x"), "o-y"} {
		_ = b.Put(ctx, n, bytes.NewReader([]byte("1")))
	}
	if err := ApplyRetention(ctx, b, []BackupSet{set("x", t0), set("z", t0)}); err != nil {
		t.Fatal(err)
	}
	objs, _ := b.List(ctx, "")
	if len(objs) != 1 || objs[0].Name != "o-y" {
		t.Fatalf("%v", objs)
	}
	p := set("y", t0)
	p.Pinned = true
	if err := ApplyRetention(ctx, b, []BackupSet{p}); err == nil {
		t.Fatal("pinned set deleted")
	}
}

type staticApprovals map[string]Approval

func (s staticApprovals) Approval(id string) (Approval, bool) { a, ok := s[id]; return a, ok }

func TestCheckerConditions(t *testing.T) {
	ctx := context.Background()
	c := &Checker{
		Backend:   backup.NewMemBackend(),
		Nodes:     func(context.Context) ([]NodeState, error) { return threeNodes(), nil },
		Policy:    SourcePolicy{Job: jobDemandTest},
		Approvals: staticApprovals{"appr-1": {ID: "appr-1", By: "alice"}},
		SessionReport: func(id string) (VerifyReport, bool) {
			return VerifyReport{Passed: id == "s-ok"}, id == "s-ok" || id == "s-bad"
		},
	}
	check := func(kind, target string) bool {
		ok, _, err := c.Check(ctx, "op", runner.Step{}, runner.Condition{Kind: kind, Target: target, ClusterSize: 3})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	cases := []struct {
		kind, target string
		want         bool
	}{
		{CondSourceEligible, "n2", true}, {CondSourceEligible, "n1", false},
		{CondApproved, "appr-1", true}, {CondApproved, "appr-2", false},
		{CondRestoreVerified, "s-ok", true}, {CondRestoreVerified, "s-bad", false}, {CondRestoreVerified, "s-none", false},
		{CondBackupComplete, "missing", false}, {CondRestoreComplete, "dbcm-job-x", false},
		{runner.CondNodeSynced, "n1", false},
	}
	for _, tc := range cases {
		if got := check(tc.kind, tc.target); got != tc.want {
			t.Fatalf("%s %s: %v", tc.kind, tc.target, got)
		}
	}
}

func TestCheckerBackupCompleteDetectsSizeMismatch(t *testing.T) {
	key := testKey(t)
	be, _, _ := runPhysicalBackup(t, key)
	ctx := context.Background()
	obj := "prod-b1.xbs.dbcms"
	_ = be.Put(ctx, obj, bytes.NewReader([]byte("short")))
	c := &Checker{Backend: be}
	ok, why, _ := c.Check(ctx, "op", runner.Step{}, runner.Condition{Kind: CondBackupComplete, Target: obj})
	if ok {
		t.Fatalf("size mismatch accepted (%s)", why)
	}
	_ = be.Delete(ctx, obj)
	if ok, _, _ := c.Check(ctx, "op", runner.Step{}, runner.Condition{Kind: CondBackupComplete, Target: obj}); ok {
		t.Fatal("missing stream accepted")
	}
}
