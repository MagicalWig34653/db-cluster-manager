// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

func ownerOf(t *testing.T, p string) (uid, gid int, mode fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode()
}

// initVolume creates an owned volume with a marker, as create-volume and
// init-identity do before init-volume-dirs.
func newNodeVolume(t *testing.T, f *fixture, name string) string {
	t.Helper()
	root := f.volume(name)
	wantStatus(t, f.mustDo(1, "op", "vol-"+name, KindCreateVolume, CreateVolumeArgs{Volume: name, VolumeRoot: root}), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "init-"+name, KindInitIdentity, InitIdentityArgs{VolumeRoot: root,
		Token: NewIdentityToken{TokenID: "tok-" + name, Identity: testIdentity()}}), StatusSucceeded, "")
	return root
}

func TestInitVolumeDirs(t *testing.T) {
	f := newFixture(t)
	root := newNodeVolume(t, f, "shop-n1")
	_, _, markerMode := ownerOf(t, filepath.Join(root, MarkerFileName))
	rootUID, rootGID, rootMode := ownerOf(t, root)
	// Unit tests do not run as root: chown to the test user's own IDs is the
	// only ownership change an unprivileged process may make.
	uid, gid := os.Getuid(), os.Getgid()
	args := InitVolumeDirsArgs{VolumeRoot: root, Expected: testIdentity(), UID: uid, GID: gid}
	r := f.mustDo(1, "op", "dirs", KindInitVolumeDirs, args)
	wantStatus(t, r, StatusSucceeded, "")
	var out struct {
		Dirs     []string `json:"dirs"`
		UID, GID int
	}
	if err := json.Unmarshal(r.Output, &out); err != nil || len(out.Dirs) != 2 {
		t.Fatalf("output %s (%v)", r.Output, err)
	}
	for _, d := range []string{galera.DataDirName, galera.RunDirName} {
		u, g, m := ownerOf(t, filepath.Join(root, d))
		if !m.IsDir() || m.Perm() != 0o750 || u != uid || g != gid {
			t.Errorf("%s: mode %v owner %d:%d", d, m, u, g)
		}
	}
	// The volume root and the marker are untouched (root-owned in
	// production; the database user cannot replace the marker).
	if u, g, m := ownerOf(t, root); u != rootUID || g != rootGID || m != rootMode {
		t.Errorf("volume root changed: %d:%d %v", u, g, m)
	}
	if _, _, m := ownerOf(t, filepath.Join(root, MarkerFileName)); m != markerMode {
		t.Errorf("marker mode changed: %v", m)
	}
	// The agent's collect-health default socket lies in the run directory.
	if filepath.Dir(filepath.Join(root, DefaultSocket)) != filepath.Join(root, galera.RunDirName) {
		t.Errorf("DefaultSocket %q not in %s", DefaultSocket, galera.RunDirName)
	}

	// Idempotent: an existing directory with a wrong mode is corrected; its
	// content is not touched.
	if err := os.Chmod(filepath.Join(root, "data"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, "data", "ibdata1"), "x")
	wantStatus(t, f.mustDo(1, "op", "dirs-again", KindInitVolumeDirs, args), StatusSucceeded, "")
	if _, _, m := ownerOf(t, filepath.Join(root, "data")); m.Perm() != 0o750 {
		t.Errorf("mode not corrected: %v", m)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "ibdata1")); err != nil {
		t.Errorf("content removed: %v", err)
	}
}

func TestInitVolumeDirsRefusals(t *testing.T) {
	f := newFixture(t)
	root := newNodeVolume(t, f, "shop-n1")
	uid, gid := os.Getuid(), os.Getgid()

	// Invalid arguments: never root, never outside the base.
	for name, a := range map[string]InitVolumeDirsArgs{
		"root uid":  {VolumeRoot: root, Expected: testIdentity(), UID: 0, GID: gid},
		"root gid":  {VolumeRoot: root, Expected: testIdentity(), UID: uid, GID: 0},
		"negative":  {VolumeRoot: root, Expected: testIdentity(), UID: -5, GID: gid},
		"huge":      {VolumeRoot: root, Expected: testIdentity(), UID: 1 << 32, GID: gid},
		"outside":   {VolumeRoot: f.dir, Expected: testIdentity(), UID: uid, GID: gid},
		"dot-dot":   {VolumeRoot: f.base + "/../x", Expected: testIdentity(), UID: uid, GID: gid},
		"relative":  {VolumeRoot: "shop-n1", Expected: testIdentity(), UID: uid, GID: gid},
		"base only": {VolumeRoot: f.base, Expected: testIdentity(), UID: uid, GID: gid},
	} {
		if _, err := f.do(1, "op", "bad-"+name, KindInitVolumeDirs, a); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Wrong identity: refused before any directory is created.
	other := testIdentity()
	other.NodeUUID = uuidB
	wantStatus(t, f.mustDo(1, "op", "foreign", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: root, Expected: other, UID: uid, GID: gid}),
		StatusRefused, CodeIdentityMismatch)
	if _, err := os.Lstat(filepath.Join(root, "data")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("data created on a mismatching identity: %v", err)
	}
	// No marker: refused (a volume without identity is never prepared).
	bare := f.volume("bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.mustDo(1, "op", "bare", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: bare, Expected: testIdentity(), UID: uid, GID: gid})
	if r.Status != StatusRefused {
		t.Fatalf("volume without marker: %+v", r)
	}

	// A symlink in place of run/ is never followed: no chown of its target.
	outside := filepath.Join(f.dir, "etc")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "symlink", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: root, Expected: testIdentity(), UID: uid, GID: gid}),
		StatusRefused, CodeInvalidPath)
	if _, _, m := ownerOf(t, outside); m.Perm() != 0o755 {
		t.Errorf("symlink target modified: %v", m)
	}
	// A relative symlink that stays inside the root is refused too.
	if err := os.Remove(filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "symlink-in", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: root, Expected: testIdentity(), UID: uid, GID: gid}),
		StatusRefused, CodeInvalidPath)
	// A regular file in place of a directory is refused.
	if err := os.Remove(filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, "run"), "")
	wantStatus(t, f.mustDo(1, "op", "file", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: root, Expected: testIdentity(), UID: uid, GID: gid}),
		StatusRefused, CodeInvalidPath)
}

// A chown the process may not make fails with an I/O code (as the agent
// running unprivileged would); the step is recorded as failed, not success.
func TestInitVolumeDirsChownFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown to another user succeeds")
	}
	f := newFixture(t)
	root := newNodeVolume(t, f, "shop-n1")
	r := f.mustDo(1, "op", "dirs", KindInitVolumeDirs, InitVolumeDirsArgs{VolumeRoot: root, Expected: testIdentity(), UID: 999, GID: 999})
	if os.Getuid() != 999 {
		wantStatus(t, r, StatusFailed, CodeIO)
	}
}
