// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func ownedService(f *fixture, t *testing.T, id, name string, replicas uint64, running *uint64) {
	t.Helper()
	f.docker.objects[id] = Object{Kind: KindService, ID: id, Name: name, Labels: map[string]string{LabelInstallation: testInstallation},
		Replicas: replicas, RunningTasks: running}
	wantStatus(t, f.mustDo(1, "op", "allow-"+id, KindAllowObjects, AllowObjectsArgs{IDs: []string{id}}), StatusSucceeded, "")
}

func TestAwaitStopped(t *testing.T) {
	f := newFixture(t)
	ownedService(f, t, "s-stopped", "dbcm-shop-n1", 0, ptr[uint64](0))
	ownedService(f, t, "s-stopping", "dbcm-shop-n2", 0, ptr[uint64](1))
	ownedService(f, t, "s-unknown", "dbcm-shop-n3", 0, nil)
	ownedService(f, t, "s-up", "dbcm-shop-n4", 1, ptr[uint64](1))
	f.docker.objects["foreign"] = Object{Kind: KindService, ID: "foreign", Name: "dbcm-shop-n5", Labels: map[string]string{LabelInstallation: testInstallation}}
	wantStatus(t, f.mustDo(1, "op", "a1", KindAwaitStopped, AwaitServiceArgs{ServiceName: "dbcm-shop-n1"}), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "a2", KindAwaitStopped, AwaitServiceArgs{ServiceName: "dbcm-shop-n2"}), StatusFailed, CodeStillRunning)
	wantStatus(t, f.mustDo(1, "op", "a3", KindAwaitStopped, AwaitServiceArgs{ServiceName: "dbcm-shop-n3"}), StatusFailed, CodeStillRunning)
	// Scaled up again after the stop: refused, the operation must not go on.
	wantStatus(t, f.mustDo(1, "op", "a4", KindAwaitStopped, AwaitServiceArgs{ServiceName: "dbcm-shop-n4"}), StatusRefused, CodeStillRunning)
	// Not in the allow-list.
	wantStatus(t, f.mustDo(1, "op", "a5", KindAwaitStopped, AwaitServiceArgs{ServiceName: "dbcm-shop-n5"}), StatusRefused, CodeNotOwned)
	wantStatus(t, f.mustDo(1, "op", "a6", KindAwaitStopped, AwaitServiceArgs{ServiceName: "nope"}), StatusFailed, CodeNotFound)
	if m := f.docker.Mutations(); len(m) != 0 {
		t.Fatalf("await-stopped changed something: %v", m)
	}
}

func TestStartService(t *testing.T) {
	f := newFixture(t)
	ownedService(f, t, "s1", "dbcm-shop-n1", 0, ptr[uint64](0))
	ownedService(f, t, "s2", "dbcm-shop-n2", 1, ptr[uint64](1))
	wantStatus(t, f.mustDo(1, "op", "st1", KindStartService, AwaitServiceArgs{ServiceName: "dbcm-shop-n1"}), StatusSucceeded, "")
	if countPrefix(f.docker.Calls(), "scale-service s1 1") != 1 {
		t.Fatalf("calls %v", f.docker.Calls())
	}
	// Already running: success without a scale call.
	wantStatus(t, f.mustDo(1, "op", "st2", KindStartService, AwaitServiceArgs{ServiceName: "dbcm-shop-n2"}), StatusSucceeded, "")
	if countPrefix(f.docker.Calls(), "scale-service s2") != 0 {
		t.Fatalf("calls %v", f.docker.Calls())
	}
	// Jobs are never started this way.
	f.docker.objects["job"] = Object{Kind: KindService, ID: "job", Name: "dbcm-job-x", Labels: map[string]string{LabelInstallation: testInstallation, LabelJob: "backup"}}
	wantStatus(t, f.mustDo(1, "op", "allow-job", KindAllowObjects, AllowObjectsArgs{IDs: []string{"job"}}), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "st3", KindStartService, AwaitServiceArgs{ServiceName: "dbcm-job-x"}), StatusRefused, CodeImmutable)
}

const grastateCrash = "# GALERA saved state\nversion: 2.1\nuuid:    6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f\nseqno:   -1\nsafe_to_bootstrap: 0\n"

func TestSetSafeToBootstrap(t *testing.T) {
	got := SetSafeToBootstrap(grastateCrash)
	want := strings.Replace(grastateCrash, "safe_to_bootstrap: 0", "safe_to_bootstrap: 1", 1)
	if got != want {
		t.Fatalf("got %q", got)
	}
	// Before grastate 2.1 the key is absent: appended.
	old := "version: 2.0\nuuid: 6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f\nseqno: 5"
	if got := SetSafeToBootstrap(old); got != old+"\nsafe_to_bootstrap: 1\n" {
		t.Fatalf("got %q", got)
	}
}

func markArgs(root string, seqno int64) MarkSafeArgs {
	return MarkSafeArgs{VolumeRoot: root, Expected: testIdentity(), UUID: "6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f", Seqno: seqno}
}

func TestMarkSafeToBootstrap(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, root, testIdentity())
	file := filepath.Join(root, "data", GrastateFileName)
	writeRaw(t, file, grastateCrash)
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}

	// The position changed since the diagnosis: refused, file untouched.
	wantStatus(t, f.mustDo(1, "op", "m0", KindMarkSafeToBootstrap, markArgs(root, 1234)), StatusRefused, CodePositionChanged)
	if string(readFile(t, file)) != grastateCrash {
		t.Fatal("file changed on a refused mark")
	}
	r := f.mustDo(1, "op", "m1", KindMarkSafeToBootstrap, markArgs(root, -1))
	wantStatus(t, r, StatusSucceeded, "")
	if got := string(readFile(t, file)); got != SetSafeToBootstrap(grastateCrash) {
		t.Fatalf("grastate = %q", got)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, err %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "data", ".dbcm-grastate-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	// Already safe: success without a change (a re-check before bootstrap).
	r = f.mustDo(1, "op", "m2", KindMarkSafeToBootstrap, markArgs(root, -1))
	wantStatus(t, r, StatusSucceeded, "")
	if !strings.Contains(string(r.Output), `"changed":"false"`) {
		t.Fatalf("output %s", r.Output)
	}
	// After a crash the recovered seqno is written as well (lab E6: a
	// bootstrap from seqno -1 started a new cluster history).
	writeRaw(t, file, grastateCrash)
	set := markArgs(root, -1)
	seq := int64(3525)
	set.SetSeqno = &seq
	wantStatus(t, f.mustDo(1, "op", "m4", KindMarkSafeToBootstrap, set), StatusSucceeded, "")
	if got := string(readFile(t, file)); !strings.Contains(got, "seqno:   3525\n") || !strings.Contains(got, "safe_to_bootstrap: 1\n") ||
		!strings.Contains(got, "uuid:    6b1f8a4c") {
		t.Fatalf("grastate = %q", got)
	}
	// Replayed with a new step after the write: the file now says 3525, not
	// the approved -1, so it is refused.
	wantStatus(t, f.mustDo(1, "op", "m5", KindMarkSafeToBootstrap, set), StatusRefused, CodePositionChanged)
	// set_seqno cannot overwrite a known seqno.
	bad := markArgs(root, 3525)
	bad.SetSeqno = &seq
	if _, err := f.do(1, "op", "m6", KindMarkSafeToBootstrap, bad); err == nil {
		t.Fatal("set_seqno over a known seqno accepted")
	}
	writeRaw(t, file, SetSafeToBootstrap(grastateCrash))
	// Another node's identity is refused before anything is read.
	other := markArgs(root, -1)
	other.Expected.NodeUUID = "559e29b2-0000-4000-8000-000000000009"
	wantStatus(t, f.mustDo(1, "op", "m3", KindMarkSafeToBootstrap, other), StatusRefused, CodeIdentityMismatch)
}

func TestMarkSafeRefusesEmptyStateAndSymlinks(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, root, testIdentity())
	file := filepath.Join(root, "data", GrastateFileName)
	// No grastate.dat at all.
	wantStatus(t, f.mustDo(1, "op", "e0", KindMarkSafeToBootstrap, markArgs(root, -1)), StatusRefused, CodePositionChanged)
	// A symbolic link to a file outside the volume is not followed.
	outside := filepath.Join(t.TempDir(), "victim")
	writeRaw(t, outside, grastateCrash)
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	r := f.mustDo(1, "op", "e1", KindMarkSafeToBootstrap, markArgs(root, -1))
	if r.Status == StatusSucceeded {
		t.Fatalf("symlink followed: %+v", r)
	}
	if string(readFile(t, outside)) != grastateCrash {
		t.Fatal("file outside the volume changed")
	}
	// Invalid arguments are rejected before execution.
	bad := markArgs(root, -1)
	bad.UUID = "00000000-0000-0000-0000-000000000000"
	if _, err := f.do(1, "op", "e2", KindMarkSafeToBootstrap, bad); err == nil {
		t.Fatal("zero UUID accepted")
	}
}
