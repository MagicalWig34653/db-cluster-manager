// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Commands of the guided full-outage recovery (galera
// PlanFullOutageRecovery, ADR 0004/A06). The manager only plans them after
// a diagnosis and an explicit, digest-bound approval; the agent still checks
// ownership, identity and the approved position itself.
const (
	// KindAwaitStopped succeeds when an owned replicated service, found by
	// name, has 0 desired replicas and no running task (args:
	// AwaitServiceArgs). It changes nothing; it fails with CodeStillRunning
	// while a task still runs (the runner retries) and is refused when the
	// service wants replicas again (someone scaled it up).
	KindAwaitStopped Kind = "await-stopped"
	// KindStartService scales an owned replicated service, found by name,
	// from 0 to 1 replica (args: AwaitServiceArgs). It runs on a Swarm
	// manager node, which usually does not hold the node's volume, so it
	// does not verify the identity marker: the recovery plan verifies every
	// marker on the volume's own host first, and update-service cannot
	// change a service's mounts or placement. Use start-db where the agent
	// runs on both the Swarm manager and the volume's host.
	KindStartService Kind = "start-service"
	// KindMarkSafeToBootstrap verifies the volume identity, then sets
	// safe_to_bootstrap: 1 in the node's grastate.dat, but only if the file
	// still holds the position the approved diagnosis saw (args:
	// MarkSafeArgs). The file is replaced atomically inside the volume root
	// (os.Root: neither ".." nor a symbolic link can leave it) with its
	// owner and mode kept.
	KindMarkSafeToBootstrap Kind = "mark-safe-to-bootstrap"
)

// Codes of the recovery commands.
const (
	// CodeStillRunning: a database task still runs (await-stopped).
	CodeStillRunning Code = "still-running"
	// CodePositionChanged: grastate.dat no longer holds the approved
	// position, or holds no Galera state (mark-safe-to-bootstrap).
	CodePositionChanged Code = "position-changed"
)

// MarkSafeArgs are the arguments of KindMarkSafeToBootstrap. UUID and Seqno
// are the position of the approved diagnosis.
type MarkSafeArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
	DataDir    string   `json:"data_dir,omitempty"`
	UUID       string   `json:"uuid"`
	Seqno      int64    `json:"seqno"`
	// SetSeqno, if set, is written as the seqno too (the wsrep-recover
	// position of a node whose grastate.dat says -1); only allowed when
	// Seqno is -1.
	SetSeqno *int64 `json:"set_seqno,omitempty"`
	// AllowZeroUUID accepts the zero cluster UUID as the approved position
	// (zero-UUID recovery, ADR 0028): the file must hold exactly UUID and
	// Seqno, both unchanged by the command; only the flag is set.
	AllowZeroUUID bool `json:"allow_zero_uuid,omitempty"`
}

func checkMarkSafe(a *MarkSafeArgs, volumeBase string) error {
	if a.DataDir != "" && !filepath.IsLocal(a.DataDir) {
		return invalid("data_dir %q must be a local relative path", a.DataDir)
	}
	if a.AllowZeroUUID {
		if a.UUID != galera.ZeroUUID || a.SetSeqno != nil {
			return invalid("allow_zero_uuid needs the zero uuid and no set_seqno")
		}
	} else if a.UUID == "" || a.UUID == galera.ZeroUUID {
		return invalid("uuid of the approved position required")
	}
	if a.Seqno < -1 {
		return invalid("seqno %d", a.Seqno)
	}
	if a.SetSeqno != nil && (a.Seqno != -1 || *a.SetSeqno < 0) {
		return invalid("set_seqno only fills an unknown seqno (-1) with a non-negative one")
	}
	return checkVolumeRoot(a.VolumeRoot, volumeBase)
}

// ownedServiceByName finds an owned service by name.
func (e *Executor) ownedServiceByName(ctx context.Context, name string) (Object, Result, bool) {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return Object{}, outcome(err), false
	}
	for _, o := range objs {
		if o.Name != name {
			continue
		}
		if err := e.owner.Check(o, KindService); err != nil {
			return Object{}, outcome(err), false
		}
		if o.Labels[LabelJob] != "" {
			return Object{}, Result{Status: StatusRefused, Code: CodeImmutable, Message: fmt.Sprintf("service %q is a job", name)}, false
		}
		return o, Result{}, true
	}
	return Object{}, outcome(fmt.Errorf("%w: service %q", ErrObjectNotFound, name)), false
}

func (e *Executor) awaitStopped(ctx context.Context, a AwaitServiceArgs) Result {
	o, res, ok := e.ownedServiceByName(ctx, a.ServiceName)
	if !ok {
		return res
	}
	if o.Replicas != 0 {
		return Result{Status: StatusRefused, Code: CodeStillRunning,
			Message: fmt.Sprintf("service %q wants %d replica(s); it was scaled up after the stop", a.ServiceName, o.Replicas)}
	}
	if o.RunningTasks == nil {
		return Result{Status: StatusFailed, Code: CodeStillRunning, Message: fmt.Sprintf("service %q: running task count unknown", a.ServiceName)}
	}
	if *o.RunningTasks > 0 {
		return Result{Status: StatusFailed, Code: CodeStillRunning, Message: fmt.Sprintf("service %q still has %d running task(s)", a.ServiceName, *o.RunningTasks)}
	}
	return succeeded(map[string]string{"id": o.ID})
}

func (e *Executor) startService(ctx context.Context, a AwaitServiceArgs) Result {
	o, res, ok := e.ownedServiceByName(ctx, a.ServiceName)
	if !ok {
		return res
	}
	if o.Replicas > 1 {
		return immutable("service %q has %d replicas; a database service has at most one", a.ServiceName, o.Replicas)
	}
	if o.Replicas == 1 {
		return succeeded(map[string]string{"id": o.ID}) // already started
	}
	if err := e.docker.ScaleService(ctx, o.ID, 1); err != nil {
		return outcome(err)
	}
	return succeeded(map[string]string{"id": o.ID})
}

// SetSafeToBootstrap returns grastate content with safe_to_bootstrap: 1
// (the line is replaced, or appended for grastate versions before 2.1).
// Everything else is kept byte for byte.
func SetSafeToBootstrap(content string) string { return setGrastate(content, nil) }

// setGrastate sets safe_to_bootstrap: 1 and, if seqno is not nil, the
// seqno line.
func setGrastate(content string, seqno *int64) string {
	var b strings.Builder
	found := false
	for line := range strings.Lines(content) {
		k, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(k) == "safe_to_bootstrap" {
			b.WriteString("safe_to_bootstrap: 1\n")
			found = true
			continue
		}
		if ok && seqno != nil && strings.TrimSpace(k) == "seqno" {
			b.WriteString("seqno:   " + strconv.FormatInt(*seqno, 10) + "\n")
			continue
		}
		b.WriteString(line)
	}
	out := b.String()
	if !found {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "safe_to_bootstrap: 1\n"
	}
	return out
}

func (e *Executor) markSafe(a MarkSafeArgs) Result {
	rootPath, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(rootPath, a.Expected); err != nil {
		return outcome(err)
	}
	dataDir := a.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	r, err := os.OpenRoot(rootPath)
	if err != nil {
		return outcome(err)
	}
	defer r.Close()
	name := filepath.Join(dataDir, GrastateFileName)
	f, err := r.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{Status: StatusRefused, Code: CodePositionChanged, Message: GrastateFileName + " does not exist"}
	}
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: err.Error()}
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return outcome(err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: GrastateFileName + " is not a regular file"}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxGrastateSize+1))
	f.Close()
	if err != nil {
		return outcome(err)
	}
	if len(b) > maxGrastateSize {
		return Result{Status: StatusFailed, Code: CodeIO, Message: fmt.Sprintf("%s larger than %d bytes", GrastateFileName, maxGrastateSize)}
	}
	g, err := galera.ParseGrastate(string(b))
	if err != nil {
		return Result{Status: StatusRefused, Code: CodePositionChanged, Message: err.Error()}
	}
	if (g.Empty() && !a.AllowZeroUUID) || g.UUID != a.UUID || g.Seqno != a.Seqno {
		return Result{Status: StatusRefused, Code: CodePositionChanged,
			Message: fmt.Sprintf("grastate.dat holds %s:%d, the approved diagnosis saw %s:%d", g.UUID, g.Seqno, a.UUID, a.Seqno)}
	}
	if g.SafeToBootstrap && a.SetSeqno == nil {
		return succeeded(map[string]string{"changed": "false"})
	}
	if err := replaceInRoot(r, name, []byte(setGrastate(string(b), a.SetSeqno)), fi); err != nil {
		return outcome(err)
	}
	return succeeded(map[string]string{"changed": "true"})
}

// replaceInRoot writes data to a new temporary file next to name, gives it
// the owner and mode of the old file (fi), syncs it and renames it over
// name, all through r.
func replaceInRoot(r *os.Root, name string, data []byte, fi fs.FileInfo) (err error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".dbcm-grastate-"+hex.EncodeToString(rnd[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = r.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, bytes.NewReader(data)); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if err = r.Chown(tmp, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err = r.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return err
	}
	if err = r.Rename(tmp, name); err != nil {
		return err
	}
	d, derr := r.Open(filepath.Dir(name))
	if derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
