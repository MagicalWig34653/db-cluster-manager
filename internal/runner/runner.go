// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// Config configures a Runner.
type Config struct {
	Leader  Leader
	Journal Journal
	Agents  AgentClient
	// Checker evaluates preconditions on live state; optional.
	Checker Checker
	// Now returns the time for timeline events; nil uses time.Now.
	Now func() time.Time
	// Sleep waits between retries; nil uses a real timer. It must return
	// ctx.Err() when ctx ends first.
	Sleep func(ctx context.Context, d time.Duration) error

	BarrierTimeout time.Duration // default 5s
	StepTimeout    time.Duration // default 60s; per command attempt
	BackoffBase    time.Duration // default 1s; doubled per retry
	BackoffMax     time.Duration // default 30s
	// Log receives retries, pauses, failures and aborts with the operation,
	// step and attempt; nil discards them.
	Log *slog.Logger
}

// Runner executes operations. It is safe for concurrent use; independent
// operations may run concurrently from different goroutines.
type Runner struct {
	cfg   Config
	mu    sync.Mutex
	ops   map[string]*opState
	locks map[string]string // lock key -> operation ID
}

type opState struct {
	def        Operation
	noDef      bool // adopted without a definition: no commands may be sent
	status     Status
	term       uint64
	reason     string
	pausedStep string
	active     bool // an execute loop or a management call owns it
	abortReq   bool
	// abortCh is closed when an abort is requested while the operation
	// executes: it interrupts retry backoff and cancels an in-flight
	// command that has no effect (Idempotent kinds).
	abortCh    chan struct{}
	locked     bool
	agentStep  map[string]string // step ID -> agent step ID in use
	fresh      map[string]bool   // agent step ID not sent yet
	lastResult map[string]*agent.Result
	timeline   []Event
}

// New returns a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Leader == nil || cfg.Journal == nil || cfg.Agents == nil {
		return nil, errors.New("runner: Leader, Journal and Agents are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = realSleep
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.BarrierTimeout, 5*time.Second)
	def(&cfg.StepTimeout, 60*time.Second)
	def(&cfg.BackoffBase, time.Second)
	def(&cfg.BackoffMax, 30*time.Second)
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Runner{cfg: cfg, ops: map[string]*opState{}, locks: map[string]string{}}, nil
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cloneOp(op Operation) Operation {
	op.Locks = slices.Clone(op.Locks)
	op.Steps = slices.Clone(op.Steps)
	for i := range op.Steps {
		op.Steps[i].Args = slices.Clone(op.Steps[i].Args)
		op.Steps[i].Preconditions = slices.Clone(op.Steps[i].Preconditions)
	}
	return op
}

func newOpState(op Operation, term uint64) *opState {
	return &opState{def: op, status: StatusRunning, term: term, abortCh: make(chan struct{}),
		agentStep: map[string]string{}, fresh: map[string]bool{}, lastResult: map[string]*agent.Result{}}
}

// requestAbortLocked records an abort request and wakes the execute loop.
// The caller holds r.mu.
func (r *Runner) requestAbortLocked(st *opState) {
	st.abortReq = true
	select {
	case <-st.abortCh:
	default:
		close(st.abortCh)
	}
}

func (r *Runner) abortRequested(st *opState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return st.abortReq
}

// abortable returns a context that also ends when an abort of st is
// requested. Lab E4 (failcreate): an abort of a create whose idempotent
// exec-sql step kept failing returned 202, but the operation stayed
// running through its retry backoff for more than 300 s.
func (r *Runner) abortable(ctx context.Context, st *opState) (context.Context, context.CancelFunc) {
	actx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-st.abortCh:
			cancel()
		case <-actx.Done():
		}
	}()
	return actx, cancel
}

// errAbortWait: a wait ended because an abort was requested; the step
// state is journaled and the operation is paused, so execute aborts it.
var errAbortWait = errors.New("runner: abort requested")

// noAbort maps errAbortWait to nil for the step loop.
func noAbort(err error) error {
	if errors.Is(err, errAbortWait) {
		return nil
	}
	return err
}

// pauseForAbort pauses st for the requested abort; execute turns the pause
// into the abort.
func (r *Runner) pauseForAbort(st *opState, stepID, why string) {
	r.setStatus(st, StatusPaused, stepID, "abort requested "+why, EventPause)
}

// lead verifies leadership and runs the barrier. It returns the term that
// owns everything this call does.
func (r *Runner) lead() (uint64, error) {
	l := r.cfg.Leader
	if !l.IsLeader() {
		return 0, ErrNotLeader
	}
	before := l.Term()
	if err := l.Barrier(r.cfg.BarrierTimeout); err != nil {
		return 0, fmt.Errorf("%w: barrier: %v", ErrNotLeader, err)
	}
	term := l.Term()
	if term == 0 || term != before || !l.IsLeader() {
		return 0, fmt.Errorf("%w: term changed during barrier", ErrNotLeader)
	}
	return term, nil
}

// checkLeader is the last check before a journal write or a command.
func (r *Runner) checkLeader(st *opState) error {
	if !r.cfg.Leader.IsLeader() {
		return fmt.Errorf("%w: no longer leader", ErrLostLeadership)
	}
	if t := r.cfg.Leader.Term(); t != st.term {
		return fmt.Errorf("%w: term %d, operation owned by %d", ErrLostLeadership, t, st.term)
	}
	return nil
}

func (r *Runner) event(st *opState, typ EventType, stepID, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventLocked(st, typ, stepID, detail)
}

func (r *Runner) eventLocked(st *opState, typ EventType, stepID, detail string) {
	st.timeline = append(st.timeline, Event{At: r.cfg.Now(), Term: st.term, Type: typ, StepID: stepID, Detail: detail})
}

// lockLocked takes all lock keys of st or none. The caller holds r.mu.
func (r *Runner) lockLocked(st *opState) error {
	if st.locked {
		return nil
	}
	for _, k := range st.def.Locks {
		if h, ok := r.locks[k]; ok && h != st.def.ID {
			return fmt.Errorf("%w: %q held by %q", ErrBusy, k, h)
		}
	}
	for _, k := range st.def.Locks {
		r.locks[k] = st.def.ID
	}
	st.locked = true
	return nil
}

func (r *Runner) unlockLocked(st *opState) {
	if !st.locked {
		return
	}
	for _, k := range st.def.Locks {
		if r.locks[k] == st.def.ID {
			delete(r.locks, k)
		}
	}
	st.locked = false
}

// setStatus records a status change; terminal and stopped states release
// the locks, paused and failed operations keep them.
func (r *Runner) setStatus(st *opState, s Status, stepID, reason string, typ EventType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st.status, st.reason = s, reason
	st.pausedStep = ""
	if s == StatusPaused {
		st.pausedStep = stepID
	}
	switch s {
	case StatusSucceeded, StatusAborted, StatusStopped:
		r.unlockLocked(st)
	}
	r.eventLocked(st, typ, stepID, reason)
	log := r.cfg.Log.With("op", st.def.ID, "kind", st.def.Kind, "step", stepID, "status", string(s), "reason", reason)
	switch s {
	case StatusFailed, StatusStopped:
		log.Warn("runner: operation " + string(s))
	case StatusPaused, StatusAborted, StatusSucceeded:
		log.Info("runner: operation " + string(s))
	}
}

func lostLeadership(err error) bool {
	return errors.Is(err, opjournal.ErrNotOwner) || errors.Is(err, metastore.ErrNotLeader) ||
		errors.Is(err, metastore.ErrOutcomeUnknown) || errors.Is(err, ErrLostLeadership) || errors.Is(err, ErrNotLeader)
}

// stop marks st Stopped after a journal or leadership error and returns
// the error for the caller.
func (r *Runner) stop(st *opState, stepID string, err error) error {
	r.setStatus(st, StatusStopped, stepID, err.Error(), EventStop)
	if lostLeadership(err) && !errors.Is(err, ErrLostLeadership) {
		return fmt.Errorf("%w: %w", ErrLostLeadership, err)
	}
	return err
}

// claim marks a registered operation active for a management call.
func (r *Runner) claim(opID string) (*opState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.ops[opID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownOperation, opID)
	}
	if st.active {
		return nil, fmt.Errorf("%w: %q", ErrActive, opID)
	}
	if st.status == StatusSucceeded || st.status == StatusAborted {
		return nil, fmt.Errorf("%w: %q is %s", ErrFinished, opID, st.status)
	}
	st.active = true
	return st, nil
}

func (r *Runner) release(st *opState) {
	r.mu.Lock()
	st.active = false
	r.mu.Unlock()
}

// Run journals op under the current term and executes it. It returns
// ErrNotLeader (nothing journaled or sent), ErrBusy, ErrInvalidOperation or
// ErrLostLeadership. Outcomes of the operation itself (succeeded, failed,
// paused) are reported in the View with a nil error.
func (r *Runner) Run(ctx context.Context, op Operation) (View, error) {
	if err := op.Validate(); err != nil {
		return View{}, err
	}
	op = cloneOp(op)
	term, err := r.lead()
	if err != nil {
		return View{}, err
	}
	st := newOpState(op, term)
	r.mu.Lock()
	if _, exists := r.ops[op.ID]; exists {
		r.mu.Unlock()
		return View{}, fmt.Errorf("%w: operation %q already registered", ErrInvalidOperation, op.ID)
	}
	if err := r.lockLocked(st); err != nil {
		r.mu.Unlock()
		return View{}, err
	}
	st.active = true
	r.ops[op.ID] = st
	r.mu.Unlock()

	if err := r.cfg.Journal.Begin(op.ID, op.Kind, op.stepIDs(), term); err != nil {
		if errors.Is(err, opjournal.ErrExists) {
			r.mu.Lock()
			r.unlockLocked(st)
			delete(r.ops, op.ID)
			r.mu.Unlock()
			return View{}, fmt.Errorf("%w: %w", ErrInvalidOperation, err)
		}
		err = r.stop(st, "", fmt.Errorf("journal begin: %w", err))
		r.release(st)
		return r.view(st), err
	}
	r.event(st, EventBegin, "", fmt.Sprintf("%d steps", len(op.Steps)))
	return r.execute(ctx, st)
}

// TakeOver is called by a new leader. It runs the barrier, adopts every
// unfinished journal operation with the current term (Running steps become
// Unknown) and registers it with the definition returned by defs. It sends
// no command; Resume continues each adopted operation. An operation without
// a matching definition is registered paused and can only be aborted.
func (r *Runner) TakeOver(defs func(opID string) (Operation, bool)) ([]string, error) {
	term, err := r.lead()
	if err != nil {
		return nil, err
	}
	unfinished, err := r.cfg.Journal.Unfinished()
	if err != nil {
		return nil, fmt.Errorf("runner: list unfinished operations: %w", err)
	}
	var adopted []string
	for _, jop := range unfinished {
		r.mu.Lock()
		st := r.ops[jop.ID]
		if st != nil && st.active {
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()
		if err := r.cfg.Journal.Adopt(jop.ID, term); err != nil {
			if lostLeadership(err) {
				return adopted, fmt.Errorf("%w: adopt %q: %w", ErrLostLeadership, jop.ID, err)
			}
			return adopted, fmt.Errorf("runner: adopt %q: %w", jop.ID, err)
		}
		if st == nil {
			def, ok := Operation{}, false
			if defs != nil {
				def, ok = defs(jop.ID)
			}
			if ok && def.Validate() == nil && slices.Equal(def.stepIDs(), stepIDsOf(jop)) && def.Kind == jop.Kind {
				st = newOpState(cloneOp(def), term)
			} else {
				st = newOpState(Operation{ID: jop.ID, Kind: jop.Kind}, term)
				st.noDef = true
			}
		}
		r.mu.Lock()
		r.ops[jop.ID] = st
		st.term = term
		reason := fmt.Sprintf("adopted by term %d; resume pending", term)
		if st.noDef {
			reason = "adopted without a matching operation definition; only abort is possible"
		} else if err := r.lockLocked(st); err != nil {
			reason = "adopted; " + err.Error()
		}
		st.status, st.reason, st.pausedStep = StatusPaused, reason, ""
		r.eventLocked(st, EventAdopt, "", reason)
		r.mu.Unlock()
		adopted = append(adopted, jop.ID)
	}
	return adopted, nil
}

func stepIDsOf(op opjournal.Operation) []string {
	ids := make([]string, len(op.Steps))
	for i, s := range op.Steps {
		ids[i] = s.ID
	}
	return ids
}

// prepare claims, verifies leadership, adopts if the term advanced and
// takes the locks. On error the claim is released.
func (r *Runner) prepare(opID string, needDef bool) (*opState, error) {
	st, err := r.claim(opID)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*opState, error) {
		r.release(st)
		return nil, err
	}
	if needDef && st.noDef {
		return fail(fmt.Errorf("%w: %q has no definition", ErrInvalidOperation, opID))
	}
	term, err := r.lead()
	if err != nil {
		return fail(err)
	}
	if term != st.term {
		if err := r.cfg.Journal.Adopt(opID, term); err != nil {
			if lostLeadership(err) {
				err = fmt.Errorf("%w: %w", ErrLostLeadership, err)
			}
			return fail(err)
		}
		r.mu.Lock()
		st.term = term
		r.eventLocked(st, EventAdopt, "", fmt.Sprintf("adopted by term %d", term))
		r.mu.Unlock()
	}
	if needDef {
		r.mu.Lock()
		err := r.lockLocked(st)
		r.mu.Unlock()
		if err != nil {
			return fail(err)
		}
	}
	return st, nil
}

// Resume continues a paused, failed-by-precondition, stopped or adopted
// operation under the current term with the usual rules: Unknown steps are
// first re-queried under the same agent step ID.
func (r *Runner) Resume(ctx context.Context, opID string) (View, error) {
	st, err := r.prepare(opID, true)
	if err != nil {
		return View{}, err
	}
	r.setStatus(st, StatusRunning, "", "resumed", EventRetry)
	return r.execute(ctx, st)
}

// Reconcile resolves an operation paused on an in-doubt step after the live
// state was inspected. ResolveNotApplied sends the step again under a new
// agent step ID; it is the only way a destructive in-doubt step is re-sent.
func (r *Runner) Reconcile(ctx context.Context, opID, stepID string, res Resolution) (View, error) {
	st, err := r.prepare(opID, res != ResolveAbort)
	if err != nil {
		return View{}, err
	}
	r.mu.Lock()
	paused := st.status == StatusPaused && st.pausedStep == stepID
	r.mu.Unlock()
	js, jerr := r.journalStep(opID, stepID)
	if !paused || jerr != nil || js.State != opjournal.Unknown {
		r.release(st)
		return r.view(st), fmt.Errorf("%w: %q step %q", ErrNotPaused, opID, stepID)
	}
	r.event(st, EventReconcile, stepID, string(res))
	switch res {
	case ResolveAbort:
		v, err := r.abortNow(st)
		r.release(st)
		return v, err
	case ResolveApplied, ResolveFailed:
		result := opjournal.Succeeded
		if res == ResolveFailed {
			result = opjournal.Failed
		}
		if err := r.cfg.Journal.StartStep(opID, stepID, "", st.term); err != nil {
			err = r.stop(st, stepID, err)
			r.release(st)
			return r.view(st), err
		}
		if err := r.cfg.Journal.FinishStep(opID, stepID, result, st.term); err != nil {
			err = r.stop(st, stepID, err)
			r.release(st)
			return r.view(st), err
		}
		r.event(st, EventFinish, stepID, string(result)+" (reconciled)")
		if res == ResolveFailed {
			r.setStatus(st, StatusFailed, stepID, "step reconciled as failed", EventFail)
			r.release(st)
			return r.view(st), nil
		}
	case ResolveNotApplied:
		r.mu.Lock()
		st.agentStep[stepID] = fmt.Sprintf("%s%sr%d", stepID, reservedSep, js.Attempts+1)
		st.fresh[stepID] = true
		r.mu.Unlock()
	default:
		r.release(st)
		return r.view(st), fmt.Errorf("%w: unknown resolution %q", ErrInvalidOperation, res)
	}
	r.setStatus(st, StatusRunning, "", "reconciled", EventRetry)
	return r.execute(ctx, st)
}

// Abort ends an operation at a safe boundary. If the operation is
// executing, the abort is recorded and takes effect before its next step,
// before its next retry (it interrupts the retry backoff) or, for a command
// without effect (Idempotent kinds), at once: that command's context is
// cancelled and its step is journaled Unknown, never Succeeded. A
// destructive command in flight is not cancelled: its answer is awaited
// (bounded by the step timeout) so that its outcome stays observable.
// Otherwise the abort is journaled now. A Running step blocks the abort.
func (r *Runner) Abort(opID string) error {
	r.mu.Lock()
	st, ok := r.ops[opID]
	switch {
	case !ok:
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownOperation, opID)
	case st.status == StatusSucceeded || st.status == StatusAborted:
		r.mu.Unlock()
		return fmt.Errorf("%w: %q is %s", ErrFinished, opID, st.status)
	case st.active:
		r.requestAbortLocked(st)
		r.eventLocked(st, EventAbortRequested, "", "abort at the next safe boundary")
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	st, err := r.prepare(opID, false)
	if err != nil {
		return err
	}
	defer r.release(st)
	_, err = r.abortNow(st)
	return err
}

func (r *Runner) abortNow(st *opState) (View, error) {
	if err := r.checkLeader(st); err != nil {
		return r.view(st), r.stop(st, "", err)
	}
	if err := r.cfg.Journal.Abort(st.def.ID, st.term); err != nil {
		if lostLeadership(err) {
			return r.view(st), r.stop(st, "", err)
		}
		return r.view(st), fmt.Errorf("runner: abort %q: %w", st.def.ID, err)
	}
	r.mu.Lock()
	st.abortReq = false
	prior, priorStep, priorReason := st.status, st.pausedStep, st.reason
	r.mu.Unlock()
	// An operator abort always ends Aborted, also when it coincides with a
	// step failure; the failure is kept in the reason (and in the journal).
	reason := "aborted at a safe boundary"
	if prior == StatusFailed && priorReason != "" {
		reason += "; a step had failed: " + priorReason
	} else if prior == StatusPaused && priorStep != "" && !strings.HasPrefix(priorReason, "abort requested") {
		reason += fmt.Sprintf("; paused at step %s: %s", priorStep, priorReason)
	}
	if jop, err := r.cfg.Journal.Get(st.def.ID); err == nil {
		var unknown []string
		for _, s := range jop.Steps {
			if s.State == opjournal.Unknown {
				unknown = append(unknown, s.ID)
			}
		}
		if len(unknown) > 0 {
			// The journal keeps these steps Unknown; an abort never records
			// them as succeeded.
			reason += fmt.Sprintf("; outcome unknown for step(s) %v (inspect live state)", unknown)
		}
	}
	r.setStatus(st, StatusAborted, "", reason, EventAbort)
	return r.view(st), nil
}

func (r *Runner) journalStep(opID, stepID string) (opjournal.Step, error) {
	op, err := r.cfg.Journal.Get(opID)
	if err != nil {
		return opjournal.Step{}, err
	}
	for _, s := range op.Steps {
		if s.ID == stepID {
			return s, nil
		}
	}
	return opjournal.Step{}, fmt.Errorf("runner: step %q not in journal of %q", stepID, opID)
}

// execute runs the steps of a claimed operation from its journal state.
func (r *Runner) execute(ctx context.Context, st *opState) (View, error) {
	defer r.release(st)
	for {
		r.mu.Lock()
		abort := st.abortReq
		r.mu.Unlock()
		if abort {
			return r.abortNow(st)
		}
		jop, err := r.cfg.Journal.Get(st.def.ID)
		if err != nil {
			return r.view(st), r.stop(st, "", fmt.Errorf("journal get: %w", err))
		}
		if !slices.Equal(stepIDsOf(jop), st.def.stepIDs()) {
			return r.view(st), r.stop(st, "", fmt.Errorf("%w: journal steps differ from definition", ErrInvalidOperation))
		}
		i := slices.IndexFunc(jop.Steps, func(s opjournal.Step) bool { return s.State != opjournal.Succeeded })
		if i < 0 {
			r.setStatus(st, StatusSucceeded, "", "all steps succeeded", EventSucceed)
			return r.view(st), nil
		}
		js, spec := jop.Steps[i], st.def.Steps[i]
		var cont bool
		switch js.State {
		case opjournal.Pending, opjournal.Unknown:
			cont, err = r.runStep(ctx, st, spec, js, 0)
		case opjournal.Failed:
			if !js.Retry {
				r.setStatus(st, StatusFailed, spec.ID, "step failed", EventFail)
				break
			}
			// The previous owner journaled the failure as having no effect
			// with a retry due, then stopped (crash, leadership change)
			// before the retry started: retry under the next agent step ID,
			// with the attempts already made counted against the budget.
			r.newAgentStep(st, spec.ID, js.Attempts+1)
			r.event(st, EventRetry, spec.ID, fmt.Sprintf("retry due after attempt %d (journaled before the take-over)", js.Attempts))
			cont, err = r.runStep(ctx, st, spec, js, js.Attempts)
		case opjournal.Running:
			r.setStatus(st, StatusPaused, spec.ID, "step is running under the current term; its completion cannot be observed here", EventPause)
		default:
			r.setStatus(st, StatusPaused, spec.ID, "unexpected step state "+string(js.State), EventPause)
		}
		if err != nil || !cont {
			r.mu.Lock()
			abort := st.abortReq && (st.status == StatusPaused || st.status == StatusFailed)
			r.mu.Unlock()
			if abort && err == nil {
				return r.abortNow(st)
			}
			return r.view(st), err
		}
	}
}

type outcome int

const (
	outSucceeded  outcome = iota
	outFailed             // the agent observed a failure or refused
	outRejected           // invalid command or step conflict: no effect, permanent
	outStale              // fencing rejected our term
	outInDoubt            // the agent cannot tell
	outUnanswered         // transport error or timeout: unknown completion
)

func classify(res agent.Result, err error) outcome {
	switch {
	case errors.Is(err, fencing.ErrStaleTerm):
		return outStale
	case errors.Is(err, agent.ErrInvalidCommand), errors.Is(err, agent.ErrStepConflict):
		return outRejected
	case errors.Is(err, agent.ErrPersist) && res.Status != "":
		// The result was observed but not made durable; adopt what was seen.
	case err != nil:
		return outUnanswered
	}
	switch res.Status {
	case agent.StatusSucceeded:
		return outSucceeded
	case agent.StatusFailed, agent.StatusRefused:
		return outFailed
	default:
		return outInDoubt
	}
}

func (r *Runner) backoff(n int) time.Duration {
	d := r.cfg.BackoffBase
	for i := 1; i < n && d < r.cfg.BackoffMax; i++ {
		d *= 2
	}
	return min(d, r.cfg.BackoffMax)
}

// wait sleeps for the n-th backoff and re-checks leadership.
func (r *Runner) wait(ctx context.Context, st *opState, stepID string, n int, why string) error {
	d := r.backoff(n)
	r.event(st, EventRetry, stepID, fmt.Sprintf("%s; retry %d after %s", why, n, d))
	r.cfg.Log.Warn("runner: step retry", "op", st.def.ID, "kind", st.def.Kind, "step", stepID, "retry", n, "backoff", d.String(), "reason", why)
	actx, cancel := r.abortable(ctx, st)
	err := r.cfg.Sleep(actx, d)
	cancel()
	if ctx.Err() == nil && r.abortRequested(st) {
		r.pauseForAbort(st, stepID, "while waiting to retry")
		return errAbortWait
	}
	if err != nil {
		return r.stop(st, stepID, fmt.Errorf("runner stopped while waiting: %w", err))
	}
	if err := r.checkLeader(st); err != nil {
		return r.stop(st, stepID, err)
	}
	return nil
}

func (r *Runner) finish(st *opState, stepID string, s opjournal.State) error {
	if err := r.cfg.Journal.FinishStep(st.def.ID, stepID, s, st.term); err != nil {
		return r.stop(st, stepID, fmt.Errorf("journal finish: %w", err))
	}
	r.event(st, EventFinish, stepID, string(s))
	return nil
}

// runStep drives one step until it succeeds (true), or the operation pauses,
// fails or stops (false).
// extra is the number of retries already used (a retry due from a previous
// owner counts the attempts it made).
func (r *Runner) runStep(ctx context.Context, st *opState, spec Step, js opjournal.Step, extra int) (bool, error) {
	opID := st.def.ID
	state := js.State
	idem := Idempotent(spec.Command)
	for {
		if r.abortRequested(st) {
			r.pauseForAbort(st, spec.ID, "before the step (re)started")
			return false, nil
		}
		if state == opjournal.Pending || spec.Command == "" {
			if ok, err := r.awaitPreconditions(ctx, st, spec); !ok || err != nil {
				return false, err
			}
		}
		if err := r.checkLeader(st); err != nil {
			return false, r.stop(st, spec.ID, err)
		}
		r.mu.Lock()
		agentID := st.agentStep[spec.ID]
		if agentID == "" && spec.Command != "" {
			// After a take-over: the journal names the agent step ID of the
			// latest attempt, so an Unknown step is queried under the ID
			// that was actually sent.
			agentID = spec.ID
			if js.AgentStep != "" {
				agentID = js.AgentStep
			}
			st.agentStep[spec.ID] = agentID
		}
		r.mu.Unlock()
		if err := r.cfg.Journal.StartStep(opID, spec.ID, agentID, st.term); err != nil {
			return false, r.stop(st, spec.ID, fmt.Errorf("journal start: %w", err))
		}
		cur, err := r.journalStep(opID, spec.ID)
		if err != nil {
			return false, r.stop(st, spec.ID, err)
		}
		r.event(st, EventStart, spec.ID, fmt.Sprintf("attempt %d", cur.Attempts))
		if spec.Command == "" {
			return r.finish(st, spec.ID, opjournal.Succeeded) == nil, r.errIfStopped(st)
		}

		r.mu.Lock()
		query := state == opjournal.Unknown && !st.fresh[spec.ID]
		st.fresh[spec.ID] = false
		r.mu.Unlock()

		// Last check: never send without leadership of the owning term. The
		// step stays Running; a new leader's Adopt turns it Unknown.
		if err := r.checkLeader(st); err != nil {
			return false, r.stop(st, spec.ID, err)
		}
		typ := EventSend
		if query {
			typ = EventQuery
		}
		r.event(st, typ, spec.ID, fmt.Sprintf("%s %s to %s", spec.Command, agentID, spec.Target))
		sendCtx, cancelSend := ctx, context.CancelFunc(func() {})
		if idem {
			// A command without effect may be cancelled by an abort.
			sendCtx, cancelSend = r.abortable(ctx, st)
		}
		res, sendErr := r.send(sendCtx, st, spec, agentID)
		cancelSend()
		r.recordResult(st, spec.ID, res, sendErr)

		switch classify(res, sendErr) {
		case outSucceeded:
			return r.finish(st, spec.ID, opjournal.Succeeded) == nil, r.errIfStopped(st)
		case outFailed, outRejected:
			retryable := idem || (sendErr == nil && res.Code != "" && slices.Contains(spec.RetryCodes, res.Code))
			if retryable && extra < spec.MaxRetries && classify(res, sendErr) == outFailed {
				// Journaled as a due retry: a new leader that takes over
				// during the backoff retries the step (lab E4
				// cleanuprestart) instead of failing the operation.
				if err := r.cfg.Journal.FailForRetry(opID, spec.ID, st.term); err != nil {
					return false, r.stop(st, spec.ID, fmt.Errorf("journal finish: %w", err))
				}
				r.event(st, EventFinish, spec.ID, string(opjournal.Failed)+" (retry due)")
				extra++
				r.newAgentStep(st, spec.ID, cur.Attempts+1)
				if err := r.wait(ctx, st, spec.ID, extra, retryReason(spec, idem, res, sendErr)); err != nil {
					return false, noAbort(err)
				}
				state = opjournal.Failed
				continue
			}
			if err := r.finish(st, spec.ID, opjournal.Failed); err != nil {
				return false, err
			}
			r.setStatus(st, StatusFailed, spec.ID, describe(res, sendErr), EventFail)
			return false, nil
		case outStale:
			return false, r.stop(st, spec.ID, fmt.Errorf("%w: agent rejected term %d: %v", ErrLostLeadership, st.term, sendErr))
		case outInDoubt:
			if err := r.finish(st, spec.ID, opjournal.Unknown); err != nil {
				return false, err
			}
			if idem && extra < spec.MaxRetries {
				extra++
				r.newAgentStep(st, spec.ID, cur.Attempts+1)
				if err := r.wait(ctx, st, spec.ID, extra, "idempotent step in doubt: "+describe(res, sendErr)); err != nil {
					return false, noAbort(err)
				}
				state = opjournal.Unknown
				continue
			}
			r.setStatus(st, StatusPaused, spec.ID, "outcome in doubt; inspect live state and Reconcile: "+describe(res, sendErr), EventPause)
			return false, nil
		case outUnanswered:
			if err := r.finish(st, spec.ID, opjournal.Unknown); err != nil {
				return false, err
			}
			if ctx.Err() != nil {
				return false, r.stop(st, spec.ID, fmt.Errorf("runner context ended: %w", ctx.Err()))
			}
			if idem && r.abortRequested(st) {
				// The abort cancelled the command; the step stays Unknown.
				r.pauseForAbort(st, spec.ID, fmt.Sprintf("while %s was in flight; it was cancelled and its outcome is unknown", spec.Command))
				return false, nil
			}
			if extra < spec.MaxRetries {
				extra++
				// Same agent step ID: the agent replays a recorded result or
				// executes a command it never received; it never runs twice.
				if err := r.wait(ctx, st, spec.ID, extra, "no answer: "+Redact(sendErr.Error())); err != nil {
					return false, noAbort(err)
				}
				state = opjournal.Unknown
				continue
			}
			r.setStatus(st, StatusPaused, spec.ID, "agent did not answer; outcome unknown: "+Redact(sendErr.Error()), EventPause)
			return false, nil
		}
	}
}

// errIfStopped returns ErrLostLeadership when a finish write stopped st.
func (r *Runner) errIfStopped(st *opState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.status == StatusStopped {
		return fmt.Errorf("%w: %s", ErrLostLeadership, st.reason)
	}
	return nil
}

func (r *Runner) newAgentStep(st *opState, stepID string, attempt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st.agentStep[stepID] = fmt.Sprintf("%s%sa%d", stepID, reservedSep, attempt)
	st.fresh[stepID] = true
}

// retryReason explains a retry of a failed step with the agent's answer
// (lab E4: only "idempotent step failed" was visible, not why). A database
// that is not running yet is named as such: the step waits for that
// dependency, not for the SQL itself.
func retryReason(spec Step, idem bool, res agent.Result, err error) string {
	d := describe(res, err)
	switch {
	case err == nil && res.Code == agent.CodeDBNotRunning:
		return fmt.Sprintf("dependency not running: the database on %s is not running: %s", spec.Target, d)
	case idem:
		return "idempotent step failed: " + d
	default:
		return "step failed without effect: " + d
	}
}

// describe renders an attempt's outcome for reasons, the timeline and the
// log. The agent already redacts the secrets it knows; Redact is a second
// line for anything that looks like a credential.
func describe(res agent.Result, err error) string {
	if err != nil {
		return Redact(err.Error())
	}
	s := string(res.Status)
	if res.Code != "" {
		s += " (" + string(res.Code) + ")"
	}
	if res.Message != "" {
		s += ": " + res.Message
	}
	return Redact(s)
}

// maxDetail bounds an error text stored in reasons and timeline entries.
const maxDetail = 1024

var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	// SQL: IDENTIFIED BY [PASSWORD] '...'
	{regexp.MustCompile(`(?i)(identified\s+by\s+(?:password\s+)?)'(?:[^'\\]|\\.)*'`), "${1}'***'"},
	// key=value or key: value for credential-like keys.
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`), "${1}${2}***"},
	// DSN user:password@unix(...) or @tcp(...).
	{regexp.MustCompile(`([\w.-]+):[^@\s/]+@(unix|tcp)\(`), "${1}:***@${2}("},
}

// Redact masks credential-looking parts of an error text and bounds its
// length.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	if len(s) > maxDetail {
		s = strings.ToValidUTF8(s[:maxDetail], "") + "..."
	}
	return s
}

func (r *Runner) recordResult(st *opState, stepID string, res agent.Result, err error) {
	detail := describe(res, err)
	if res.Runtime != "" {
		target := ""
		for _, sp := range st.def.Steps {
			if sp.ID == stepID {
				target = sp.Target
			}
		}
		detail += " [executed by runtime=" + string(res.Runtime) + " host=" + target + "]"
	}
	if err == nil && res.Replayed {
		detail += " [replayed]"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil || res.Status != "" {
		rc := res
		rc.Output = slices.Clone(res.Output)
		st.lastResult[stepID] = &rc
	}
	r.eventLocked(st, EventResult, stepID, detail)
}

func (r *Runner) send(ctx context.Context, st *opState, spec Step, agentID string) (agent.Result, error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = r.cfg.StepTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := agent.Command{Term: st.term, OpID: st.def.ID, StepID: agentID, Kind: spec.Command, Args: json.RawMessage(slices.Clone(spec.Args))}
	return r.cfg.Agents.Send(cctx, spec.Target, cmd)
}

// awaitPreconditions polls the step's preconditions up to MaxRetries times
// with backoff, then pauses the operation.
func (r *Runner) awaitPreconditions(ctx context.Context, st *opState, spec Step) (bool, error) {
	for polls := 0; ; polls++ {
		// Waiting for preconditions is a safe boundary: nothing has been
		// sent for this step. Lab E4: an abort requested during a long
		// gate wait (up to MaxRetries polls) otherwise took minutes.
		r.mu.Lock()
		abort := st.abortReq
		r.mu.Unlock()
		if abort {
			r.setStatus(st, StatusPaused, spec.ID, "abort requested while waiting for preconditions", EventPause)
			return false, nil
		}
		reason, fatal := r.unmet(ctx, st, spec)
		if reason == "" {
			return true, nil
		}
		if fatal {
			return false, r.failGate(st, spec, reason)
		}
		if polls >= spec.MaxRetries {
			r.setStatus(st, StatusPaused, spec.ID, "precondition not met: "+reason, EventPause)
			return false, nil
		}
		r.event(st, EventWait, spec.ID, reason)
		if err := r.wait(ctx, st, spec.ID, polls+1, "precondition not met"); err != nil {
			return false, noAbort(err)
		}
	}
}

// failGate fails a gate whose condition can never hold: the gate step is
// journaled Failed and the operation Failed with the reason (locks are kept
// like for any failed step).
func (r *Runner) failGate(st *opState, spec Step, reason string) error {
	if err := r.checkLeader(st); err != nil {
		return r.stop(st, spec.ID, err)
	}
	r.mu.Lock()
	agentID := st.agentStep[spec.ID]
	r.mu.Unlock()
	if err := r.cfg.Journal.StartStep(st.def.ID, spec.ID, agentID, st.term); err != nil {
		return r.stop(st, spec.ID, fmt.Errorf("journal start: %w", err))
	}
	if err := r.finish(st, spec.ID, opjournal.Failed); err != nil {
		return err
	}
	r.setStatus(st, StatusFailed, spec.ID, reason, EventFail)
	return nil
}

// unmet returns why the preconditions do not hold ("" when they do). fatal
// is set when a condition reported ErrConditionFatal.
func (r *Runner) unmet(ctx context.Context, st *opState, spec Step) (reason string, fatal bool) {
	for _, c := range spec.Preconditions {
		if c.Kind == CondStepSucceeded {
			s, err := r.journalStep(st.def.ID, c.StepID)
			if err != nil {
				return err.Error(), false
			}
			if s.State != opjournal.Succeeded {
				return fmt.Sprintf("step %s is %s", c.StepID, s.State), false
			}
			continue
		}
		if r.cfg.Checker == nil {
			return fmt.Sprintf("no checker for condition %s", c.Kind), false
		}
		ok, why, err := r.cfg.Checker.Check(ctx, st.def.ID, spec, c)
		if errors.Is(err, ErrConditionFatal) {
			msg := err.Error()
			if why != "" {
				msg = why
			}
			return fmt.Sprintf("%s %s: %s", c.Kind, c.Target, Redact(msg)), true
		}
		if err != nil {
			return fmt.Sprintf("%s: check failed: %v", c.Kind, err), false
		}
		if !ok {
			if why == "" {
				why = "not satisfied"
			}
			return fmt.Sprintf("%s %s: %s", c.Kind, c.Target, why), false
		}
	}
	return "", false
}

// Snapshot returns the observable state of a registered operation.
func (r *Runner) Snapshot(opID string) (View, error) {
	r.mu.Lock()
	st, ok := r.ops[opID]
	r.mu.Unlock()
	if !ok {
		return View{}, fmt.Errorf("%w: %q", ErrUnknownOperation, opID)
	}
	return r.view(st), nil
}

// List returns snapshots of all registered operations sorted by ID.
func (r *Runner) List() []View {
	r.mu.Lock()
	ids := make([]string, 0, len(r.ops))
	for id := range r.ops {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	sort.Strings(ids)
	out := make([]View, 0, len(ids))
	for _, id := range ids {
		if v, err := r.Snapshot(id); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (r *Runner) view(st *opState) View {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Read the journal while holding the state lock. Otherwise a snapshot
	// could read a Running step, then observe Aborted after the execution
	// goroutine has journaled Unknown, and publish an incoherent view.
	jop, jerr := r.cfg.Journal.Get(st.def.ID)
	v := View{ID: st.def.ID, Kind: st.def.Kind, Status: st.status, Term: st.term, Reason: st.reason,
		PausedStep: st.pausedStep, AbortRequested: st.abortReq, Locks: slices.Clone(st.def.Locks),
		Timeline: slices.Clone(st.timeline)}
	byID := map[string]opjournal.Step{}
	if jerr == nil {
		for _, s := range jop.Steps {
			byID[s.ID] = s
		}
	}
	steps := st.def.Steps
	if st.noDef && jerr == nil {
		for _, s := range jop.Steps {
			v.Steps = append(v.Steps, StepView{ID: s.ID, State: s.State, Attempts: s.Attempts})
		}
		return v
	}
	for _, s := range steps {
		js := byID[s.ID]
		sv := StepView{ID: s.ID, Target: s.Target, Command: s.Command, State: js.State, Attempts: js.Attempts, AgentStepID: st.agentStep[s.ID]}
		if lr := st.lastResult[s.ID]; lr != nil {
			c := *lr
			sv.LastResult = &c
			sv.Runtime = lr.Runtime
		}
		v.Steps = append(v.Steps, sv)
	}
	return v
}
