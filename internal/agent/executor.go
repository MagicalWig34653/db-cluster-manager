// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// Handler is the transport-agnostic entry point for manager commands.
type Handler interface {
	Handle(ctx context.Context, cmd Command) (Result, error)
}

// Config configures an Executor.
type Config struct {
	// InstallationID is the installation this agent serves. Objects must
	// carry it in their dbcm.installation label.
	InstallationID string
	// StatePath is the JSON state file holding step outcomes, runtime
	// allow-list additions and consumed tokens.
	StatePath string
	// VolumeBase is a clean absolute directory; every volume root must lie
	// strictly below it.
	VolumeBase string
	// AllowedIDs is the initial allow-list (the manager's inventory).
	AllowedIDs []string
	Guard      *fencing.Guard
	Docker     DockerAPI
	// NodeID is this host's Swarm node ID. Backup and restore jobs are
	// pinned to it; without it run-backup and run-restore are refused.
	NodeID string
	// Status answers collect-health; nil refuses it (not-configured).
	Status StatusQuerier
	// SQL runs exec-sql templates; nil refuses them (not-configured).
	SQL SQLExecutor
	// SecretsDir is where the agent's own Swarm secrets are mounted
	// (default SecretsDir). exec-sql reads passwords from it by secret name.
	SecretsDir string
	// ResticAllowedPaths are operator-owned absolute source roots. An empty
	// list disables host-file backups. They are local policy, not command data.
	ResticAllowedPaths []string
	ResticRunner       restic.Runner
}

// StatusQuerier runs SHOW GLOBAL STATUS LIKE 'wsrep_%' against a local
// database endpoint. network is "tcp" (address host:port) or "unix"
// (address = socket path). Errors must not contain credentials.
type StatusQuerier interface {
	Status(ctx context.Context, network, address string) (map[string]string, error)
}

// Executor executes fenced, idempotent commands. Commands are serialized:
// admission and execution of one command happen under one lock, so a newer
// term cannot be admitted between the term check and the effect of an older
// command.
type Executor struct {
	mu         sync.Mutex
	cfg        Config
	volumeBase string // VolumeBase with symlinks resolved
	guard      *fencing.Guard
	docker     DockerAPI
	owner      *OwnershipGuard
	state      *persistentState
	// deny is the distributed deny list for TLS handshakes (denylist.go).
	deny atomic.Pointer[map[string]bool]
}

var _ Handler = (*Executor)(nil)

// NewExecutor loads the state file and returns an Executor.
func NewExecutor(cfg Config) (*Executor, error) {
	switch {
	case cfg.InstallationID == "":
		return nil, errors.New("agent: InstallationID required")
	case cfg.StatePath == "":
		return nil, errors.New("agent: StatePath required")
	case cfg.Guard == nil:
		return nil, errors.New("agent: fencing Guard required")
	case cfg.Docker == nil:
		return nil, errors.New("agent: DockerAPI required")
	case !filepath.IsAbs(cfg.VolumeBase) || filepath.Clean(cfg.VolumeBase) != cfg.VolumeBase:
		return nil, fmt.Errorf("agent: VolumeBase %q must be a clean absolute path", cfg.VolumeBase)
	}
	if cfg.SecretsDir == "" {
		cfg.SecretsDir = SecretsDir
	}
	if !filepath.IsAbs(cfg.SecretsDir) || filepath.Clean(cfg.SecretsDir) != cfg.SecretsDir {
		return nil, fmt.Errorf("agent: SecretsDir %q must be a clean absolute path", cfg.SecretsDir)
	}
	for _, p := range cfg.ResticAllowedPaths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return nil, fmt.Errorf("agent: invalid restic allowed path")
		}
	}
	resolvedBase, err := filepath.EvalSymlinks(cfg.VolumeBase)
	if err != nil {
		return nil, fmt.Errorf("agent: resolve VolumeBase: %w", err)
	}
	st, err := loadState(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	owner, err := NewOwnershipGuard(cfg.InstallationID, append(append([]string{}, cfg.AllowedIDs...), st.Allowed...))
	if err != nil {
		return nil, err
	}
	e := &Executor{cfg: cfg, volumeBase: resolvedBase, guard: cfg.Guard, docker: cfg.Docker, owner: owner, state: st}
	m := denySet(st.Denied)
	e.deny.Store(&m)
	return e, nil
}

// Handle validates cmd, admits its term, and either returns the recorded
// outcome of (OpID, StepID) or executes the step and records its outcome.
//
// It returns an error, without effect and without a record, for an invalid
// command (ErrInvalidCommand), a stale term (fencing.ErrStaleTerm), a reused
// step ID with different content (ErrStepConflict), a cancelled context
// before execution, or a failure to write the intent record (ErrPersist).
// Every executed step returns a Result whose Status tells the outcome.
func (e *Executor) Handle(ctx context.Context, cmd Command) (Result, error) {
	d, err := decodeCommand(cmd, e.cfg.VolumeBase)
	if err != nil {
		return Result{}, err
	}
	if ReadOnly(cmd.Kind) {
		return e.handleRead(ctx, d)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Fencing comes first: nothing below may run for a stale term.
	if err := e.guard.Admit(cmd.Term); err != nil {
		return Result{}, err
	}
	if rec := e.state.lookup(cmd.OpID, cmd.StepID); rec != nil {
		if rec.Kind != cmd.Kind || rec.ArgsDigest != d.digest {
			return Result{}, fmt.Errorf("%w: op %q step %q", ErrStepConflict, cmd.OpID, cmd.StepID)
		}
		if rec.Result == nil {
			return Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: StatusUnknown,
				Code: CodeInDoubt, Message: "step started earlier but its outcome was not recorded; reconcile and use a new step ID",
				Replayed: true}, nil
		}
		r := *rec.Result
		r.Replayed = true
		return r, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	// Write-ahead intent record: after a crash the step is in doubt, never
	// silently executed twice.
	rec := &stepRecord{Term: cmd.Term, Kind: cmd.Kind, ArgsDigest: d.digest}
	e.state.put(cmd.OpID, cmd.StepID, rec)
	if err := e.state.save(e.cfg.StatePath); err != nil {
		e.state.drop(cmd.OpID, cmd.StepID)
		return Result{}, fmt.Errorf("%w: %v", ErrPersist, err)
	}

	res := e.execute(ctx, d)
	res.OpID, res.StepID, res.Kind = cmd.OpID, cmd.StepID, cmd.Kind
	rec.Result = &res
	if err := e.state.save(e.cfg.StatePath); err != nil {
		rec.Result = nil
		return res, fmt.Errorf("%w: outcome of op %q step %q: %v", ErrPersist, cmd.OpID, cmd.StepID, err)
	}
	return res, nil
}

// handleRead admits a read-only command under the executor lock (so a newer
// term is never admitted in the middle of another command's effect) and
// then runs it without the lock and without a step record.
func (e *Executor) handleRead(ctx context.Context, d decoded) (Result, error) {
	e.mu.Lock()
	err := e.guard.Admit(d.cmd.Term)
	e.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var res Result
	switch a := d.args.(type) {
	case *CollectHealthArgs:
		res = e.collectHealth(ctx, *a)
	case *ReadGrastateArgs:
		res = e.readGrastate(*a)
	default:
		res = Result{Status: StatusFailed, Code: CodeIO, Message: "internal: unhandled read-only args type"}
	}
	res.OpID, res.StepID, res.Kind = d.cmd.OpID, d.cmd.StepID, d.cmd.Kind
	return res, nil
}

func (e *Executor) execute(ctx context.Context, d decoded) Result {
	switch a := d.args.(type) {
	case *ConfigSpec:
		return e.createConfig(ctx, d.cmd, *a)
	case *RemoveConfigArgs:
		return e.removeConfig(ctx, *a)
	case *ServiceSpec:
		return e.createService(ctx, d.cmd, *a)
	case *ServiceArgs:
		if d.cmd.Kind == KindRemoveService {
			return e.removeService(ctx, a.ServiceID)
		}
		return e.scale(ctx, a.ServiceID, 0)
	case *StartDBArgs:
		return e.startDB(ctx, *a)
	case *RemoveVolumeArgs:
		return e.removeVolume(ctx, *a)
	case *InitIdentityArgs:
		return e.initIdentity(*a)
	case *VerifyIdentityArgs:
		root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
		if !ok {
			return res
		}
		if err := Verify(root, a.Expected); err != nil {
			return outcome(err)
		}
		return succeeded(nil)
	case *InspectArgs:
		obj, res, ok := e.owned(ctx, a.ObjectKind, a.ID)
		if !ok {
			return res
		}
		return succeeded(obj)
	case *ListOwnedArgs:
		return e.listOwned(ctx, a.ObjectKind)
	case *CreateVolumeArgs:
		return e.createVolume(ctx, d.cmd, *a)
	case *UpdateServiceArgs:
		return e.updateService(ctx, *a)
	case *RunBackupArgs:
		return e.runBackup(ctx, d.cmd, *a)
	case *ResticArgs:
		return e.runRestic(ctx, d.cmd.Kind, *a)
	case *RunRestoreArgs:
		return e.runRestore(ctx, d.cmd, *a)
	case *RemoveJobArgs:
		return e.removeJob(ctx, *a)
	case *InitVolumeDirsArgs:
		return e.initVolumeDirs(*a)
	case *ExecSQLArgs:
		return e.execSQL(ctx, *a)
	case *AwaitServiceArgs:
		switch d.cmd.Kind {
		case KindAwaitStopped:
			return e.awaitStopped(ctx, *a)
		case KindStartService:
			return e.startService(ctx, *a)
		}
		return e.awaitService(ctx, *a)
	case *RemoveClusterServiceArgs:
		return e.removeClusterService(ctx, *a)
	case *RemoveSecretArgs:
		return e.removeSecret(ctx, *a)
	case *CheckRestoreTargetArgs:
		return e.checkRestoreTarget(ctx, *a)
	case *MarkSafeArgs:
		return e.markSafe(*a)
	case *WsrepRecoverArgs:
		return e.wsrepRecover(ctx, *a)
	case *SetDenyListArgs:
		return e.setDenyList(*a)
	case *AllowObjectsArgs:
		for _, id := range a.IDs {
			e.allow(id)
		}
		return succeeded(nil)
	}
	return Result{Status: StatusFailed, Code: CodeIO, Message: "internal: unhandled args type"}
}

func (e *Executor) createService(ctx context.Context, cmd Command, spec ServiceSpec) Result {
	if got, ok := spec.Labels[LabelInstallation]; ok && got != e.cfg.InstallationID {
		return outcome(fmt.Errorf("%w: spec label %s=%q, want %q", ErrNotOwned, LabelInstallation, got, e.cfg.InstallationID))
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelInstallation] = e.cfg.InstallationID
	labels[LabelOpID] = cmd.OpID
	labels[LabelStepID] = cmd.StepID
	spec.Labels = labels
	var res Result
	var ok bool
	if spec.Networks, res, ok = e.ownedRefs(ctx, KindNetwork, spec.Networks); !ok {
		return res
	}
	if spec.Secrets, res, ok = e.ownedRefs(ctx, KindSecret, spec.Secrets); !ok {
		return res
	}
	if spec.Configs, res, ok = e.ownedConfigs(ctx, spec.Configs); !ok {
		return res
	}
	id, err := e.docker.CreateService(ctx, spec)
	if err != nil {
		return outcome(err)
	}
	e.allow(id)
	return succeeded(map[string]string{"id": id})
}

// createVolume creates the local volume of one node.
//
// Mechanism: a Docker volume (driver local, type=none, o=bind) whose device
// is the host directory VolumeBase/<volume>. A Docker volume rather than a
// bare directory because the galera service spec mounts the node's data by
// volume name, the volume gets an ID in the allow-list and ownership labels
// that the guard can check, and remove-volume already operates on Docker
// volumes. The directory is created here (Docker requires it for a bind
// device) and must be new or empty; its identity marker is written only by
// init-identity with a token, never here.
//
// Idempotence per step: an existing volume that is allow-listed, carries the
// installation label and has the same mountpoint is success; an existing
// foreign volume is refused; an existing non-empty directory without a
// volume is refused as unknown data.
func (e *Executor) createVolume(ctx context.Context, cmd Command, a CreateVolumeArgs) Result {
	if got, ok := a.Labels[LabelInstallation]; ok && got != e.cfg.InstallationID {
		return outcome(fmt.Errorf("%w: volume label %s=%q, want %q", ErrNotOwned, LabelInstallation, got, e.cfg.InstallationID))
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	out := map[string]string{"id": a.Volume, "root": a.VolumeRoot}
	existing, err := e.docker.Inspect(ctx, KindVolume, a.Volume)
	switch {
	case err == nil:
		if err := e.owner.Check(existing, KindVolume); err != nil {
			return outcome(err)
		}
		if existing.Mountpoint == "" || filepath.Clean(existing.Mountpoint) != a.VolumeRoot {
			return Result{Status: StatusRefused, Code: CodeVolumeMismatch,
				Message: fmt.Sprintf("volume %q mountpoint %q is not %s", a.Volume, existing.Mountpoint, a.VolumeRoot)}
		}
		return succeeded(out)
	case !errors.Is(err, ErrObjectNotFound):
		return outcome(err)
	}
	if fi, err := os.Lstat(a.VolumeRoot); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: fmt.Sprintf("volume root %s is a symbolic link", a.VolumeRoot)}
	}
	if err := ensureEmptyDir(root); err != nil {
		return outcome(err)
	}
	labels := maps.Clone(a.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelInstallation] = e.cfg.InstallationID
	labels[LabelOpID] = cmd.OpID
	labels[LabelStepID] = cmd.StepID
	if err := e.docker.CreateVolume(ctx, VolumeSpec{Name: a.Volume, Labels: labels, Device: a.VolumeRoot}); err != nil {
		return outcome(err)
	}
	e.allow(a.Volume)
	return succeeded(out)
}

// ensureEmptyDir creates dir (mode 0755) or accepts an existing empty one.
func ensureEmptyDir(dir string) error {
	if err := os.Mkdir(dir, 0o755); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s exists and is not empty", ErrUnknownData, dir)
	}
	return nil
}

// ownershipLabels are the labels update-service may not change.
var ownershipLabels = []string{LabelInstallation, LabelCluster, LabelNode}

// updateService replaces the spec of an owned service found by name. It
// refuses changes to what defines ownership or identity: the installation,
// cluster and node labels, the node constraint (a move is a different
// operation), the mounts (they select the data volume) and the replica count
// (starting a database goes through start-db, which verifies the identity).
// The creating step's labels (dbcm.op, dbcm.step) are preserved.
// awaitService reports the update state of an owned service by name.
func (e *Executor) awaitService(ctx context.Context, a AwaitServiceArgs) Result {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err)
	}
	for _, o := range objs {
		if o.Name != a.ServiceName {
			continue
		}
		if err := e.owner.Check(o, KindService); err != nil {
			return outcome(err)
		}
		out := map[string]string{"update_state": o.UpdateState}
		switch o.UpdateState {
		case "", "completed":
			return succeeded(out)
		case "updating", "rollback_started":
			return Result{Status: StatusFailed, Code: CodeUpdatePending, Message: fmt.Sprintf("service %q update state %q", a.ServiceName, o.UpdateState)}
		default:
			return Result{Status: StatusFailed, Code: CodeUpdateFailed, Message: fmt.Sprintf("service %q update state %q", a.ServiceName, o.UpdateState)}
		}
	}
	return outcome(fmt.Errorf("%w: service %q", ErrObjectNotFound, a.ServiceName))
}

func (e *Executor) updateService(ctx context.Context, a UpdateServiceArgs) Result {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err)
	}
	var cur Object
	found := false
	for _, o := range objs {
		if o.Name != a.ServiceName {
			continue
		}
		if err := e.owner.Check(o, KindService); err != nil {
			return outcome(err)
		}
		cur, found = o, true
		break
	}
	if !found {
		return outcome(fmt.Errorf("%w: service %q", ErrObjectNotFound, a.ServiceName))
	}
	cur, res, ok := e.owned(ctx, KindService, cur.ID)
	if !ok {
		return res
	}
	spec := a.Spec
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for _, k := range ownershipLabels {
		if v, set := labels[k]; set && v != cur.Labels[k] {
			return immutable("label %s of service %q is %q, the update sets %q", k, a.ServiceName, cur.Labels[k], v)
		}
		if v, has := cur.Labels[k]; has {
			labels[k] = v
		}
	}
	if cur.Labels[LabelJob] != "" {
		return immutable("service %q is a job; jobs are not updated", a.ServiceName)
	}
	for _, k := range []string{LabelOpID, LabelStepID} {
		if v, has := cur.Labels[k]; has {
			labels[k] = v
		}
	}
	spec.Labels = labels
	if !slices.Equal(spec.Constraints, cur.Constraints) {
		return immutable("placement constraints of service %q are %v, the update sets %v", a.ServiceName, cur.Constraints, spec.Constraints)
	}
	if !slices.Equal(spec.Mounts, cur.Mounts) {
		return immutable("mounts of service %q cannot be changed", a.ServiceName)
	}
	if spec.Replicas != cur.Replicas {
		return immutable("replicas of service %q are %d, the update sets %d (use start-db or stop-db)", a.ServiceName, cur.Replicas, spec.Replicas)
	}
	if spec.Networks, res, ok = e.ownedRefs(ctx, KindNetwork, spec.Networks); !ok {
		return res
	}
	if spec.Secrets, res, ok = e.ownedRefs(ctx, KindSecret, spec.Secrets); !ok {
		return res
	}
	if spec.Configs, res, ok = e.ownedConfigs(ctx, spec.Configs); !ok {
		return res
	}
	if err := e.docker.UpdateService(ctx, cur.ID, spec); err != nil {
		return outcome(err)
	}
	return succeeded(map[string]string{"id": cur.ID})
}

func immutable(format string, a ...any) Result {
	return Result{Status: StatusRefused, Code: CodeImmutable, Message: fmt.Sprintf(format, a...)}
}

func (e *Executor) removeService(ctx context.Context, id string) Result {
	if _, res, ok := e.owned(ctx, KindService, id); !ok {
		return res
	}
	if err := e.docker.RemoveService(ctx, id); err != nil {
		return outcome(err)
	}
	e.forget(id)
	return succeeded(nil)
}

func (e *Executor) scale(ctx context.Context, id string, replicas uint64) Result {
	if _, res, ok := e.owned(ctx, KindService, id); !ok {
		return res
	}
	if err := e.docker.ScaleService(ctx, id, replicas); err != nil {
		return outcome(err)
	}
	return succeeded(nil)
}

func (e *Executor) startDB(ctx context.Context, a StartDBArgs) Result {
	svc, res, ok := e.owned(ctx, KindService, a.ServiceID)
	if !ok {
		return res
	}
	if mounted, res, ok := e.mountsVolume(ctx, svc, a.VolumeRoot); !ok {
		return res
	} else if !mounted {
		return Result{Status: StatusRefused, Code: CodeVolumeMismatch,
			Message: fmt.Sprintf("service %q has no mount at or below %s", a.ServiceID, a.VolumeRoot)}
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	if err := e.docker.ScaleService(ctx, a.ServiceID, 1); err != nil {
		return outcome(err)
	}
	return succeeded(nil)
}

func (e *Executor) removeVolume(ctx context.Context, a RemoveVolumeArgs) Result {
	absent := succeeded(map[string]string{"removed": "absent"})
	if a.MissingOK && !e.owner.Allowed(a.VolumeID) {
		// Only a volume that does not exist at all counts as removed; an
		// existing one outside the allow-list stays refused below.
		if _, err := e.docker.Inspect(ctx, KindVolume, a.VolumeID); errors.Is(err, ErrObjectNotFound) {
			return absent
		}
	}
	vol, res, ok := e.owned(ctx, KindVolume, a.VolumeID)
	if !ok {
		if a.MissingOK && res.Code == CodeNotFound {
			e.forget(a.VolumeID)
			return absent
		}
		return res
	}
	if vol.Mountpoint == "" || filepath.Clean(vol.Mountpoint) != a.VolumeRoot {
		return Result{Status: StatusRefused, Code: CodeVolumeMismatch,
			Message: fmt.Sprintf("volume %q mountpoint %q is not %s", a.VolumeID, vol.Mountpoint, a.VolumeRoot)}
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil && !(a.AllowUnmarked && errors.Is(err, ErrEmptyVolume)) {
		return outcome(err)
	}
	if err := e.docker.RemoveVolume(ctx, a.VolumeID); err != nil {
		if errors.Is(err, ErrConflict) {
			// Docker refused without effect: a container (for example the
			// task of a service removed just before) still mounts it.
			return Result{Status: StatusFailed, Code: CodeVolumeInUse, Message: err.Error()}
		}
		return outcome(err)
	}
	e.forget(a.VolumeID)
	return succeeded(nil)
}

func (e *Executor) initIdentity(a InitIdentityArgs) Result {
	if uuid, used := e.state.ConsumedTokens[a.Token.TokenID]; used {
		return Result{Status: StatusRefused, Code: CodeTokenConsumed,
			Message: fmt.Sprintf("new-identity token %q was already used for node %s", a.Token.TokenID, uuid)}
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Initialize(root, a.Token); err != nil {
		return outcome(err)
	}
	e.state.ConsumedTokens[a.Token.TokenID] = a.Token.Identity.NodeUUID
	return succeeded(nil)
}

func (e *Executor) listOwned(ctx context.Context, kind ObjectKind) Result {
	objs, err := e.docker.ListByLabel(ctx, kind, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err)
	}
	out := []Object{}
	for _, o := range objs {
		if e.owner.Check(o, kind) == nil {
			out = append(out, o)
		}
	}
	return succeeded(out)
}

// owned applies the ownership guard: allow-list first (no Docker call for
// unrelated IDs), then inspect and label/kind check.
func (e *Executor) owned(ctx context.Context, kind ObjectKind, id string) (Object, Result, bool) {
	if !e.owner.Allowed(id) {
		return Object{}, outcome(fmt.Errorf("%w: %s %q not in allow-list", ErrNotOwned, kind, id)), false
	}
	obj, err := e.docker.Inspect(ctx, kind, id)
	if err != nil {
		return Object{}, outcome(err), false
	}
	if err := e.owner.Check(obj, kind); err != nil {
		return Object{}, outcome(err), false
	}
	return obj, Result{}, true
}

// ownedRefs checks the networks or secrets a service spec references. A
// reference may be a name or an ID, so it is resolved by an inspect first;
// the resolved object must then pass the full ownership check (ID in the
// allow-list, kind, installation label). The returned references are the
// checked object IDs, so the service is created against exactly the objects
// that were checked, not against whatever a name resolves to later.
func (e *Executor) ownedRefs(ctx context.Context, kind ObjectKind, refs []string) ([]string, Result, bool) {
	if len(refs) == 0 {
		return refs, Result{}, true
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		obj, err := e.docker.Inspect(ctx, kind, ref)
		if err != nil {
			return nil, outcome(fmt.Errorf("%s %q: %w", kind, ref, err)), false
		}
		if err := e.owner.Check(obj, kind); err != nil {
			return nil, outcome(fmt.Errorf("referenced %s %q: %w", kind, ref, err)), false
		}
		out = append(out, obj.ID)
	}
	return out, Result{}, true
}

// ownedConfigs applies the ownership guard to every referenced Swarm config
// and pins each reference to the ID that was checked, like ownedRefs does
// for networks and secrets.
func (e *Executor) ownedConfigs(ctx context.Context, refs []ConfigRef) ([]ConfigRef, Result, bool) {
	if len(refs) == 0 {
		return refs, Result{}, true
	}
	out := make([]ConfigRef, 0, len(refs))
	for _, ref := range refs {
		obj, err := e.docker.Inspect(ctx, KindConfig, ref.Config)
		if err != nil {
			return nil, outcome(fmt.Errorf("%s %q: %w", KindConfig, ref.Config, err)), false
		}
		if err := e.owner.Check(obj, KindConfig); err != nil {
			return nil, outcome(fmt.Errorf("referenced %s %q: %w", KindConfig, ref.Config, err)), false
		}
		ref.Config = obj.ID
		out = append(out, ref)
	}
	return out, Result{}, true
}

// resolveVolumeRoot resolves symlinks and re-checks that the volume root is
// below the (resolved) volume base. A missing leaf is allowed; its parent
// must resolve below the base.
func (e *Executor) resolveVolumeRoot(p string) (string, Result, bool) {
	resolved, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		var parent string
		parent, err = filepath.EvalSymlinks(filepath.Dir(p))
		if err == nil {
			resolved = filepath.Join(parent, filepath.Base(p))
		}
	}
	if err != nil {
		return "", Result{Status: StatusRefused, Code: CodeInvalidPath, Message: err.Error()}, false
	}
	if !within(e.volumeBase, resolved) {
		return "", Result{Status: StatusRefused, Code: CodeInvalidPath,
			Message: fmt.Sprintf("volume root %s resolves to %s outside the volume base", p, resolved)}, false
	}
	return resolved, Result{}, true
}

// mountsVolume reports whether svc mounts the volume root. A bind mount
// matches by path (at or below root). A volume mount names a Docker volume
// (the Galera builder mounts the node's volume by name): it matches only
// when that volume passes the ownership guard and its mountpoint is root.
// A volume mount of an unowned or missing volume does not match; any other
// failure to inspect it is returned (ok false).
func (e *Executor) mountsVolume(ctx context.Context, svc Object, root string) (mounted bool, res Result, ok bool) {
	for _, m := range svc.Mounts {
		if m.Type == "volume" {
			vol, r, owned := e.owned(ctx, KindVolume, m.Source)
			if !owned {
				if r.Code == CodeNotOwned || r.Code == CodeNotFound {
					continue
				}
				return false, r, false
			}
			if vol.Mountpoint != "" && filepath.Clean(vol.Mountpoint) == root {
				return true, Result{}, true
			}
			continue
		}
		src := filepath.Clean(m.Source)
		if src == root || within(root, src) {
			return true, Result{}, true
		}
	}
	return false, Result{}, true
}

func (e *Executor) allow(id string) {
	if id == "" || e.owner.Allowed(id) {
		return
	}
	e.owner.Allow(id)
	e.state.Allowed = append(e.state.Allowed, id)
}

func (e *Executor) forget(id string) {
	e.owner.Forget(id)
	kept := e.state.Allowed[:0]
	for _, x := range e.state.Allowed {
		if x != id {
			kept = append(kept, x)
		}
	}
	e.state.Allowed = kept
}

func succeeded(output any) Result {
	r := Result{Status: StatusSucceeded}
	if output != nil {
		b, err := json.Marshal(output)
		if err != nil {
			return Result{Status: StatusFailed, Code: CodeIO, Message: "encode output: " + err.Error()}
		}
		r.Output = b
	}
	return r
}

// outcome maps an error to a step Result.
func outcome(err error) Result {
	r := Result{Message: err.Error()}
	switch {
	case errors.Is(err, ErrOutcomeUnknown), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		r.Status, r.Code = StatusUnknown, CodeInDoubt
	case errors.Is(err, ErrNotOwned):
		r.Status, r.Code = StatusRefused, CodeNotOwned
	case errors.Is(err, ErrObjectNotFound):
		r.Status, r.Code = StatusFailed, CodeNotFound
	case errors.Is(err, ErrIdentityNotConfigured):
		r.Status, r.Code = StatusRefused, CodeIdentityNotConfigured
	case errors.Is(err, ErrForeignMarker):
		r.Status, r.Code = StatusRefused, CodeForeignMarker
	case errors.Is(err, ErrIdentityMismatch):
		r.Status, r.Code = StatusRefused, CodeIdentityMismatch
	case errors.Is(err, ErrMarkerCorrupt):
		r.Status, r.Code = StatusRefused, CodeMarkerCorrupt
	case errors.Is(err, ErrUnknownData):
		r.Status, r.Code = StatusRefused, CodeUnknownData
	case errors.Is(err, ErrEmptyVolume):
		r.Status, r.Code = StatusRefused, CodeEmptyVolume
	case isIdentityIO(err):
		r.Status, r.Code = StatusFailed, CodeIO
	default:
		r.Status, r.Code = StatusFailed, CodeDocker
	}
	return r
}

func isIdentityIO(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe)
}
