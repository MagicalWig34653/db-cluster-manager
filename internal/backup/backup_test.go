// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustKey(t *testing.T) RecoveryKey {
	t.Helper()
	k, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testMeta() Meta {
	return Meta{
		Created:            time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		SourceInstallation: "inst-1",
		MetaTerm:           7,
		MetaIndex:          1234,
	}
}

func testSnapshot() Snapshot {
	return Snapshot{
		Users:       []Record{{ID: "u1", Data: []byte(`{"name":"alice"}`)}, {ID: "u2", Data: []byte(`{"name":"bob"}`)}},
		Permissions: []Record{{ID: "p1", Data: []byte("x")}},
		Clusters:    []Record{{ID: "c1", Data: []byte("cluster-one")}, {ID: "c2", Data: []byte("cluster-two")}},
		Secrets:     []Secret{{ID: "s1", Value: []byte("ENCRYPTED-SECRET-VALUE")}},
		Settings:    map[string]string{"lang": "de"},
	}
}

func TestKeyStringRoundTrip(t *testing.T) {
	k := mustKey(t)
	s := k.String()
	if strings.ToUpper(s) != s || !strings.Contains(s, "-") {
		t.Fatalf("unexpected format %q", s)
	}
	got, err := ParseRecoveryKey(strings.ToLower(s) + " ")
	if err != nil || got != k {
		t.Fatalf("parse: %v", err)
	}
}

func TestKeyStringErrors(t *testing.T) {
	k := mustKey(t)
	s := k.String()
	// Change one data character to a different valid base32 character.
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, err := ParseRecoveryKey(string(b)); !errors.Is(err, ErrKeyChecksum) {
		t.Fatalf("want checksum error, got %v", err)
	}
	for _, bad := range []string{"", "ABC", s[:len(s)-6], "!!!!!-?????"} {
		if _, err := ParseRecoveryKey(bad); !errors.Is(err, ErrKeyFormat) {
			t.Fatalf("%q: want format error, got %v", bad, err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	key := mustKey(t)
	data, err := CreateBackup(testSnapshot(), testMeta(), key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("alice")) || bytes.Contains(data, []byte("ENCRYPTED-SECRET")) {
		t.Fatal("plaintext leaked into container")
	}
	snap, h, err := decodeSnapshot(data, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Users) != 2 || snap.Settings["lang"] != "de" {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	if h.Version != FormatVersion || h.KeyID != key.ID() || h.Meta.MetaTerm != 7 ||
		h.Meta.MetaIndex != 1234 || h.Meta.SourceInstallation != "inst-1" ||
		h.Meta.KDF != KDFRaw || h.Meta.Cipher != CipherAES256GCM || !h.Meta.Created.Equal(testMeta().Created) {
		t.Fatalf("header mismatch: %+v", h)
	}
	rh, err := ReadHeader(data)
	if err != nil || rh.KeyID != key.ID() {
		t.Fatalf("ReadHeader: %v %+v", err, rh)
	}
}

func TestWrongKey(t *testing.T) {
	data, _ := CreateBackup(testSnapshot(), testMeta(), mustKey(t))
	if _, _, err := Open(data, mustKey(t)); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v", err)
	}
}

func TestTamperEveryByte(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	for i := range data {
		c := append([]byte(nil), data...)
		c[i] ^= 0x01
		if _, _, err := Open(c, key); err == nil {
			t.Fatalf("flip at byte %d not detected", i)
		}
	}
}

func TestTruncated(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	for n := 0; n < len(data); n++ {
		if _, _, err := Open(data[:n], key); err == nil {
			t.Fatalf("truncation to %d not detected", n)
		}
	}
	if _, _, err := Open(append(append([]byte(nil), data...), 0), key); err == nil {
		t.Fatal("appended byte not detected")
	}
}

func TestVersionAndMagic(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	c := append([]byte(nil), data...)
	binary.BigEndian.PutUint16(c[len(magic):], 2)
	if _, _, err := Open(c, key); !errors.Is(err, ErrVersion) {
		t.Fatalf("got %v", err)
	}
	if _, err := ReadHeader(c); !errors.Is(err, ErrVersion) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := Open([]byte("not a backup at all"), key); !errors.Is(err, ErrNotBackup) {
		t.Fatalf("got %v", err)
	}
}

func TestRewrap(t *testing.T) {
	oldK, newK := mustKey(t), mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), oldK)
	re, err := Rewrap(data, oldK, newK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(re, newK); err != nil {
		t.Fatalf("new key: %v", err)
	}
	if _, err := Inspect(re, oldK); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("old key still works: %v", err)
	}
	h, _ := ReadHeader(re)
	if h.KeyID != newK.ID() {
		t.Fatal("key id not updated")
	}
	if _, err := Rewrap(data, newK, oldK); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("rewrap with wrong key: %v", err)
	}
}

func TestInspectRedacted(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	sum, err := Inspect(data, key)
	if err != nil {
		t.Fatal(err)
	}
	c := sum.Counts()
	if c[SectionUsers] != 2 || c[SectionPermissions] != 1 || c[SectionClusters] != 2 || c[SectionSecrets] != 1 || c[SectionSettings] != 1 {
		t.Fatalf("counts: %v", c)
	}
	if sum.Header.Meta.MetaTerm != 7 {
		t.Fatal("term missing")
	}
	if sum.Secrets[0].ID != "s1" || strings.Contains(sum.Secrets[0].Digest, "ENCRYPTED") {
		t.Fatalf("secret summary: %+v", sum.Secrets)
	}
	if _, err := Inspect(data, mustKey(t)); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func item(id, content string) Item { return Item{ID: id, Digest: Digest([]byte(content))} }

func TestPlanRestoreDiff(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	sum, _ := Inspect(data, key)
	live := LiveState{
		Users:    []Item{item("u1", `{"name":"alice"}`), item("u3", "carol")},
		Clusters: []Item{item("c1", "changed"), item("c2", "cluster-two")},
		MaxTerm:  9,
	}
	p := PlanRestore(sum, live)
	want := map[string]ChangeKind{
		"users/u2": ChangeAdd, "users/u3": ChangeRemove, "clusters/c1": ChangeModify,
		"permissions/p1": ChangeAdd, "secrets/s1": ChangeAdd, "settings/lang": ChangeAdd,
	}
	got := map[string]ChangeKind{}
	for _, c := range p.Changes {
		got[c.Section+"/"+c.ID] = c.Kind
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q want %q", k, got[k], v)
		}
	}
	if p.BackupTerm != 7 || p.LiveTerm != 9 {
		t.Fatalf("terms %d %d", p.BackupTerm, p.LiveTerm)
	}
	if PlanRestore(sum, live).Digest() != p.Digest() {
		t.Fatal("digest not stable")
	}
}

func TestRestoreInspectionModeAndApproval(t *testing.T) {
	key := mustKey(t)
	data, _ := CreateBackup(testSnapshot(), testMeta(), key)
	sum, _ := Inspect(data, key)
	plan := PlanRestore(sum, LiveState{MaxTerm: 9})
	res := BeginRestore(plan)
	if res.Mode != ModeInspection || !res.ApprovalRequired {
		t.Fatalf("%+v", res)
	}
	if res.RequiredTerm != 10 {
		t.Fatalf("required term %d, want 10 (live 9 > backup 7)", res.RequiredTerm)
	}
	if BeginRestore(PlanRestore(sum, LiveState{MaxTerm: 2})).RequiredTerm != 8 {
		t.Fatal("must exceed backup term")
	}
	good := Approval{ApprovedBy: "admin", PlanDigest: plan.Digest(), NewTerm: 10}
	bad := []Approval{
		{PlanDigest: plan.Digest(), NewTerm: 10},
		{ApprovedBy: "a", PlanDigest: "other", NewTerm: 10},
		{ApprovedBy: "a", PlanDigest: plan.Digest(), NewTerm: 9},
		{ApprovedBy: "a", PlanDigest: plan.Digest(), NewTerm: 0},
	}
	for i, a := range bad {
		if _, _, err := ConfirmResume(res, a); !errors.Is(err, ErrApproval) {
			t.Fatalf("bad %d accepted: %v", i, err)
		}
	}
	mode, term, err := ConfirmResume(res, good)
	if err != nil || mode != ModeResumed || term != 10 {
		t.Fatalf("%v %v %v", mode, term, err)
	}
	if res.Mode != ModeInspection {
		t.Fatal("result mutated")
	}
}

func e(name string, t time.Time) Entry { return Entry{Name: name, Created: t} }

func TestRetention(t *testing.T) {
	base := time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC) // Monday
	var es []Entry
	for i := 0; i < 20; i++ { // two per day, 10 days
		d := base.AddDate(0, 0, -i/2)
		es = append(es, e(d.Add(-time.Duration(i%2)*time.Hour).Format("20060102T15"), d.Add(-time.Duration(i%2)*time.Hour)))
	}
	names := func(x []Entry) string {
		var s []string
		for _, v := range x {
			s = append(s, v.Name)
		}
		return strings.Join(s, ",")
	}

	keep, rem := Select(es, Policy{})
	if len(keep) != 20 || len(rem) != 0 {
		t.Fatal("empty policy must keep all")
	}
	keep, rem = Select(es, Policy{KeepLast: 3})
	if len(keep) != 3 || len(rem) != 17 || keep[0].Created.Before(keep[1].Created) {
		t.Fatalf("last: %s", names(keep))
	}
	keep, _ = Select(es, Policy{KeepDaily: 3})
	if len(keep) != 3 || keep[0].Name != "20261019T23" {
		t.Fatalf("daily: %s", names(keep))
	}
	keep, _ = Select(es, Policy{KeepWeekly: 2})
	// Monday 19th is its own ISO week; the previous week's newest is Sunday 18th 23h.
	if names(keep) != "20261019T23,20261018T23" {
		t.Fatalf("weekly: %s", names(keep))
	}
	keep, rem = Select(es, Policy{KeepLast: 1, KeepDaily: 2, KeepWeekly: 2})
	if len(keep) != 2 || len(keep)+len(rem) != 20 {
		t.Fatalf("union: %s", names(keep))
	}
	orig := names(es)
	Select(es, Policy{KeepLast: 1})
	if names(es) != orig {
		t.Fatal("input modified")
	}
	if k, r := Select(nil, Policy{KeepLast: 2}); len(k) != 0 || len(r) != 0 {
		t.Fatal("nil input")
	}
}

func testBackend(t *testing.T, b Backend) {
	ctx := context.Background()
	if _, err := b.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if err := b.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", ".."} {
		if err := b.Put(ctx, bad, strings.NewReader("x")); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q: %v", bad, err)
		}
	}
	for _, n := range []string{"mgr-2", "mgr-1", "other"} {
		if err := b.Put(ctx, n, strings.NewReader("data-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Put(ctx, "mgr-1", strings.NewReader("replaced")); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(ctx, "mgr-1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "replaced" {
		t.Fatalf("got %q", got)
	}
	l, err := b.List(ctx, "mgr-")
	if err != nil || len(l) != 2 || l[0].Name != "mgr-1" || l[1].Name != "mgr-2" || l[0].Size != 8 {
		t.Fatalf("list: %v %+v", err, l)
	}
	if err := b.Delete(ctx, "mgr-1"); err != nil {
		t.Fatal(err)
	}
	if l, _ = b.List(ctx, ""); len(l) != 2 {
		t.Fatalf("after delete: %+v", l)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.Put(cctx, "c", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled put: %v", err)
	}
	if _, err := b.Get(cctx, "other"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled get: %v", err)
	}
}

func TestMemBackend(t *testing.T) { testBackend(t, NewMemBackend()) }

func TestDirBackend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	b, err := NewDirBackend(dir)
	if err != nil {
		t.Fatal(err)
	}
	testBackend(t, b)
	des, _ := os.ReadDir(dir)
	for _, d := range des {
		if strings.HasPrefix(d.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %s", d.Name())
		}
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestDirBackendFailedPutKeepsOldAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewDirBackend(dir)
	ctx := context.Background()
	_ = b.Put(ctx, "x", strings.NewReader("old"))
	if err := b.Put(ctx, "x", failReader{}); err == nil {
		t.Fatal("expected error")
	}
	rc, _ := b.Get(ctx, "x")
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "old" {
		t.Fatalf("old content lost: %q", got)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 1 {
		t.Fatalf("leftover files: %v", des)
	}
}
