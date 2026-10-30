// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
)

// KindSnapshotData copies a stopped node's data directory to
// <volume root>/snapshots/<name>/ on the same volume before a
// full-cluster recovery changes anything (args: SnapshotDataArgs). The
// copy keeps modes, owners, times and symbolic links; it is complete only
// once its marker file exists. The command is idempotent: a complete
// snapshot of that name is kept as it is, an incomplete one is removed and
// copied again.
//
// It refuses with CodeInsufficientSpace when the volume's file system has
// less free space than the data plus a margin (or when the free space
// cannot be determined): the recovery then does not start, and the
// operator frees space or takes the copy elsewhere. The agent verifies the
// identity marker first. The manager plans the step only after the node's
// service was stopped, so the copy is consistent; the agent additionally
// refuses while a one-off container on this host mounts the volume.
const KindSnapshotData Kind = "snapshot-data"

// Snapshot layout and codes.
const (
	// SnapshotDir is the directory below the volume root that holds the
	// snapshots. It is outside the datadir, so the database never sees it.
	SnapshotDir = "snapshots"
	// SnapshotCompleteFile marks a finished snapshot inside its directory.
	SnapshotCompleteFile = ".dbcm-snapshot-complete"
	// CodeInsufficientSpace: the volume has too little free space for the
	// snapshot.
	CodeInsufficientSpace Code = "insufficient-space"

	// snapshotMargin is the free space kept in addition to the data size.
	snapshotMargin = 64 << 20
)

var snapshotNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SnapshotDataArgs are the arguments of KindSnapshotData.
type SnapshotDataArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
	DataDir    string   `json:"data_dir,omitempty"` // relative to the volume root; default data
	Name       string   `json:"name"`
}

// SnapshotDataOutput is the output of KindSnapshotData.
type SnapshotDataOutput struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Files   int    `json:"files"`
	Existed bool   `json:"existed,omitempty"` // a complete snapshot was already there
}

func checkSnapshotData(a *SnapshotDataArgs, volumeBase string) error {
	if !snapshotNameRe.MatchString(a.Name) {
		return invalid("snapshot name %q", a.Name)
	}
	if a.DataDir != "" && !filepath.IsLocal(a.DataDir) {
		return invalid("data_dir %q must be a local relative path", a.DataDir)
	}
	if a.DataDir == SnapshotDir || filepath.Clean(a.DataDir) == SnapshotDir {
		return invalid("data_dir must not be the snapshot directory")
	}
	return checkVolumeRoot(a.VolumeRoot, volumeBase)
}

// diskFree returns the bytes available to unprivileged writers below dir.
func (e *Executor) diskFree(dir string) (uint64, error) {
	if e.cfg.DiskFree != nil {
		return e.cfg.DiskFree(dir)
	}
	return statfsFree(dir)
}

func (e *Executor) snapshotData(ctx context.Context, a SnapshotDataArgs) Result {
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	dataDir := a.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return outcome(err)
	}
	defer r.Close()
	rel := path.Join(SnapshotDir, a.Name)
	out := SnapshotDataOutput{Name: a.Name, Path: filepath.Join(root, SnapshotDir, a.Name)}
	if _, err := r.Lstat(path.Join(rel, SnapshotCompleteFile)); err == nil {
		out.Existed = true
		return succeeded(out)
	}
	var size int64
	var files int
	err = fs.WalkDir(r.FS(), dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			size += fi.Size()
			files++
		}
		return nil
	})
	if err != nil {
		return Result{Status: StatusFailed, Code: CodeIO, Message: "measure data directory: " + err.Error()}
	}
	need := uint64(size) + uint64(size)/10 + snapshotMargin
	free, err := e.diskFree(root)
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInsufficientSpace,
			Message: fmt.Sprintf("free space of %s cannot be determined (%v); no snapshot was taken", root, err)}
	}
	if free < need {
		return Result{Status: StatusRefused, Code: CodeInsufficientSpace,
			Message: fmt.Sprintf("%s has %d bytes free, the snapshot needs about %d; free space or copy the data directory elsewhere before the recovery", root, free, need)}
	}
	// Rebuild from scratch: an earlier attempt may have stopped halfway.
	if err := r.RemoveAll(rel); err != nil {
		return outcome(err)
	}
	if err := r.MkdirAll(rel, 0o700); err != nil {
		return outcome(err)
	}
	copied, err := copyTree(ctx, r, dataDir, path.Join(rel, path.Base(dataDir)))
	if err != nil {
		_ = r.RemoveAll(rel)
		if ctx.Err() != nil {
			return Result{Status: StatusUnknown, Code: CodeInDoubt, Message: ctx.Err().Error()}
		}
		return Result{Status: StatusFailed, Code: CodeIO, Message: "snapshot copy: " + err.Error()}
	}
	f, err := r.OpenFile(path.Join(rel, SnapshotCompleteFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return outcome(err)
	}
	if err = f.Sync(); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		return outcome(err)
	}
	out.Bytes, out.Files = copied, files
	return succeeded(out)
}

// copyTree copies src to dst inside r, keeping modes, owners, times and
// symbolic links; special files are skipped (a database directory holds
// none that matter once the server is stopped).
func copyTree(ctx context.Context, r *os.Root, src, dst string) (int64, error) {
	var total int64
	err := fs.WalkDir(r.FS(), src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := path.Join(dst, filepath.ToSlash(rel))
		fi, err := r.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := r.Mkdir(target, fi.Mode().Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			return keepMeta(r, target, fi, false)
		case fi.Mode()&fs.ModeSymlink != 0:
			l, err := r.Readlink(p)
			if err != nil {
				return err
			}
			if err := r.Symlink(l, target); err != nil {
				return err
			}
			return keepMeta(r, target, fi, true)
		case fi.Mode().IsRegular():
			in, err := r.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := r.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(out, in)
			if err == nil {
				err = out.Sync()
			}
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			total += n
			return keepMeta(r, target, fi, false)
		}
		return nil
	})
	return total, err
}

// keepMeta gives the copy the mode, owner and modification time of the
// original. Ownership needs privilege; the agent runs as root where it
// matters, and a refusal for another user is not an error of the copy.
func keepMeta(r *os.Root, name string, fi fs.FileInfo, link bool) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if err := r.Lchown(name, int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, fs.ErrPermission) {
			return err
		}
	}
	if link {
		return nil
	}
	if err := r.Chmod(name, fi.Mode().Perm()); err != nil {
		return err
	}
	return r.Chtimes(name, fi.ModTime(), fi.ModTime())
}
