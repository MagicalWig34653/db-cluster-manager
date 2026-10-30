// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotFixture(t *testing.T) (*fixture, string, SnapshotDataArgs) {
	t.Helper()
	f := newFixture(t)
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "mysql"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"grastate.dat":     "version: 2.1\nuuid: 00000000-0000-0000-0000-000000000000\nseqno: -1\nsafe_to_bootstrap: 0\n",
		"ibdata1":          strings.Repeat("x", 4096),
		"mysql/user.MAD":   "rows",
		"ib_logfile0":      "log",
		"mysql/empty.file": "",
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("ibdata1", filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	f.exec.cfg.DiskFree = func(string) (uint64, error) { return 1 << 40, nil }
	return f, root, SnapshotDataArgs{VolumeRoot: root, Expected: testIdentity(), Name: "pre-recovery-op-1"}
}

func TestSnapshotDataCopiesTheDataDirectoryIdempotently(t *testing.T) {
	f, root, a := snapshotFixture(t)
	r := f.mustDo(1, "op", "s1", KindSnapshotData, a)
	wantStatus(t, r, StatusSucceeded, "")
	snap := filepath.Join(root, SnapshotDir, a.Name)
	for _, rel := range []string{"data/grastate.dat", "data/ibdata1", "data/mysql/user.MAD", "data/mysql/empty.file", SnapshotCompleteFile} {
		if _, err := os.Lstat(filepath.Join(snap, rel)); err != nil {
			t.Fatalf("snapshot lacks %s: %v", rel, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(snap, "data", "ibdata1"))
	if err != nil || len(got) != 4096 {
		t.Fatalf("copied ibdata1: %d bytes, %v", len(got), err)
	}
	if fi, err := os.Stat(filepath.Join(snap, "data", "ibdata1")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode kept: %v %v", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(snap, "data", "link")); err != nil || l != "ibdata1" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	// The live data is untouched.
	if _, err := os.Stat(filepath.Join(root, "data", "grastate.dat")); err != nil {
		t.Fatal(err)
	}
	// A repeated step (new step ID, same snapshot name) keeps the first copy.
	if err := os.WriteFile(filepath.Join(root, "data", "ibdata1"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "s2", KindSnapshotData, a), StatusSucceeded, "")
	if got, _ := os.ReadFile(filepath.Join(snap, "data", "ibdata1")); len(got) != 4096 {
		t.Fatal("a completed snapshot was overwritten")
	}
}

func TestSnapshotDataRedoesAnIncompleteCopy(t *testing.T) {
	f, root, a := snapshotFixture(t)
	snap := filepath.Join(root, SnapshotDir, a.Name)
	if err := os.MkdirAll(filepath.Join(snap, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "data", "stale.partial"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "s1", KindSnapshotData, a), StatusSucceeded, "")
	if _, err := os.Stat(filepath.Join(snap, "data", "stale.partial")); err == nil {
		t.Fatal("partial leftovers survived")
	}
	if _, err := os.Stat(filepath.Join(snap, SnapshotCompleteFile)); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDataRefusesWithoutSpace(t *testing.T) {
	f, root, a := snapshotFixture(t)
	f.exec.cfg.DiskFree = func(string) (uint64, error) { return 1024, nil }
	r := f.mustDo(1, "op", "s1", KindSnapshotData, a)
	wantStatus(t, r, StatusRefused, CodeInsufficientSpace)
	if _, err := os.Stat(filepath.Join(root, SnapshotDir)); err == nil {
		t.Fatal("a refused snapshot left files behind")
	}
}

func TestSnapshotDataRefusesWithoutAFreeSpaceProbe(t *testing.T) {
	f, _, a := snapshotFixture(t)
	f.exec.cfg.DiskFree = func(string) (uint64, error) { return 0, os.ErrInvalid }
	wantStatus(t, f.mustDo(1, "op", "s1", KindSnapshotData, a), StatusRefused, CodeInsufficientSpace)
}

func TestSnapshotDataChecksNameIdentityAndRoot(t *testing.T) {
	f, root, a := snapshotFixture(t)
	for _, name := range []string{"", "../x", "a/b", ".hidden", strings.Repeat("a", 100)} {
		bad := a
		bad.Name = name
		if _, err := f.do(1, "op", "n-"+name, KindSnapshotData, bad); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	wrong := a
	wrong.Expected.NodeUUID = "00000000-0000-4000-8000-00000000dead"
	if r, err := f.do(1, "op", "id", KindSnapshotData, wrong); err != nil {
		t.Fatal(err)
	} else if r.Status == StatusSucceeded {
		t.Fatal("snapshot of a foreign identity")
	}
	outside := a
	outside.VolumeRoot = filepath.Dir(root)
	if _, err := f.do(1, "op", "out", KindSnapshotData, outside); err == nil {
		t.Fatal("volume base itself accepted")
	}
	if _, err := os.Stat(filepath.Join(root, SnapshotDir)); err == nil {
		t.Fatal("refused commands created files")
	}
}
