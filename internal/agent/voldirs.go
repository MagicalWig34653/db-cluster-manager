// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// DefaultSocket is the database's unix socket relative to the volume root
// (galera.SocketRelPath: the Galera spec passes --socket=/dbcm/run/mysqld.sock
// and mounts the volume at /dbcm). collect-health and exec-sql use it.
const DefaultSocket = galera.SocketRelPath

// dbDirMode is the mode of data/ and run/: the database user owns them, its
// group may traverse, nobody else (the agent runs as root).
const dbDirMode fs.FileMode = 0o750

// initVolumeDirs creates data/ and run/ below a verified volume root and
// gives them to the database user. All access goes through an os.Root at
// the volume root, so neither ".." nor a symbolic link can leave it; an
// existing entry must be a real directory (Lstat, never followed), and the
// ownership change is made on the opened directory itself (fchown,
// fchmod), so swapping the entry after the check cannot redirect it. Only
// the two directories change: the volume root and the marker keep their
// owner, so the database user cannot replace the marker. Content of an
// existing directory is never touched (the image entrypoint takes care of
// files in the datadir).
func (e *Executor) initVolumeDirs(a InitVolumeDirsArgs) Result {
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return outcome(err)
	}
	defer r.Close()
	dirs := []string{galera.DataDirName, galera.RunDirName}
	for _, name := range dirs {
		if res, ok := ownDir(r, name, a.UID, a.GID); !ok {
			return res
		}
	}
	if err := syncDir(root); err != nil {
		return outcome(err)
	}
	return succeeded(map[string]any{"dirs": dirs, "uid": a.UID, "gid": a.GID})
}

func ownDir(r *os.Root, name string, uid, gid int) (Result, bool) {
	refuse := func(format string, a ...any) (Result, bool) {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: fmt.Sprintf(format, a...)}, false
	}
	if err := r.Mkdir(name, dbDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return outcome(err), false
	}
	before, err := r.Lstat(name)
	if err != nil {
		return outcome(err), false
	}
	if !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return refuse("%s in the volume is not a directory (mode %v)", name, before.Mode())
	}
	f, err := r.Open(name)
	if err != nil {
		return refuse("open %s: %v", name, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return outcome(err), false
	}
	if !os.SameFile(before, opened) {
		return refuse("%s changed while it was prepared", name)
	}
	if err := f.Chown(uid, gid); err != nil {
		return outcome(err), false
	}
	if err := f.Chmod(dbDirMode); err != nil {
		return outcome(err), false
	}
	return Result{}, true
}
