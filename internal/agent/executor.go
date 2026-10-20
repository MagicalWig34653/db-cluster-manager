// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"sync"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
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
	return &Executor{cfg: cfg, volumeBase: resolvedBase, guard: cfg.Guard, docker: cfg.Docker, owner: owner, state: st}, nil
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

func (e *Executor) execute(ctx context.Context, d decoded) Result {
	switch a := d.args.(type) {
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
	id, err := e.docker.CreateService(ctx, spec)
	if err != nil {
		return outcome(err)
	}
	e.allow(id)
	return succeeded(map[string]string{"id": id})
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
	if !mountsVolume(svc, a.VolumeRoot) {
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
	vol, res, ok := e.owned(ctx, KindVolume, a.VolumeID)
	if !ok {
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
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	if err := e.docker.RemoveVolume(ctx, a.VolumeID); err != nil {
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

func mountsVolume(svc Object, root string) bool {
	for _, m := range svc.Mounts {
		src := filepath.Clean(m.Source)
		if src == root || within(root, src) {
			return true
		}
	}
	return false
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
