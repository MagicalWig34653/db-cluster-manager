// SPDX-License-Identifier: AGPL-3.0-only

// Package opjournal is an in-memory journal of long-running operations made of
// ordered steps. Each operation is owned by the fencing term of the manager
// leader that runs it. A new leader adopts unfinished operations; steps that
// were Running become Unknown because their completion could not be observed.
// The journal is a pure state machine; replication is layered on top.
// Snapshot and Restore export and import its complete state as JSON.
package opjournal

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// State is the state of a step.
type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Unknown   State = "unknown" // completion could not be observed
	Aborted   State = "aborted"
)

var (
	// ErrNotOwner is returned when a term does not own the operation.
	ErrNotOwner = errors.New("opjournal: term does not own operation")
	// ErrExists is returned when an operation ID is already used.
	ErrExists = errors.New("opjournal: operation exists")
	// ErrNotFound is returned when an operation does not exist.
	ErrNotFound = errors.New("opjournal: operation not found")
)

// Step is one ordered unit of work of an operation.
type Step struct {
	ID       string
	State    State
	Attempts int
	// AgentStep is the agent step ID of the latest attempt, recorded when
	// the step started (StartStepAs). A new leader queries the agent under
	// it for a step whose outcome is Unknown. Empty: the step's own ID.
	AgentStep string `json:",omitempty"`
	// Retry marks a Failed step whose failure had no effect and whose retry
	// was due (FailForRetry): a new leader retries it instead of failing
	// the operation. StartStep clears it.
	Retry bool `json:",omitempty"`
}

// Operation is a journaled long-running operation.
type Operation struct {
	ID    string
	Kind  string
	Owner uint64 // term of the controller that currently owns the operation
	Steps []Step
}

type entry struct {
	op      Operation
	aborted bool
}

// Journal holds operations. It is safe for concurrent use.
type Journal struct {
	mu  sync.Mutex
	ops map[string]*entry
}

// New returns an empty Journal.
func New() *Journal { return &Journal{ops: make(map[string]*entry)} }

func clone(op Operation) Operation {
	op.Steps = append([]Step(nil), op.Steps...)
	return op
}

// Begin creates an operation owned by term with all steps Pending.
// It returns ErrExists if id is already used.
func (j *Journal) Begin(id, kind string, stepIDs []string, term uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.ops[id]; ok {
		return fmt.Errorf("%w: %q", ErrExists, id)
	}
	steps := make([]Step, len(stepIDs))
	for i, s := range stepIDs {
		steps[i] = Step{ID: s, State: Pending}
	}
	j.ops[id] = &entry{op: Operation{ID: id, Kind: kind, Owner: term, Steps: steps}}
	return nil
}

// owned returns the entry for opID if term is its Owner. The caller must hold j.mu.
func (j *Journal) owned(opID string, term uint64) (*entry, error) {
	e, ok := j.ops[opID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, opID)
	}
	if term != e.op.Owner {
		return nil, fmt.Errorf("%w: got %d, owner %d", ErrNotOwner, term, e.op.Owner)
	}
	return e, nil
}

func stepIndex(e *entry, stepID string) (int, error) {
	for i, s := range e.op.Steps {
		if s.ID == stepID {
			return i, nil
		}
	}
	return 0, fmt.Errorf("opjournal: unknown step %q in operation %q", stepID, e.op.ID)
}

// StartStep moves a Pending, Unknown or Failed step to Running and increments
// Attempts. It returns ErrNotOwner if term != Owner. Steps start in order: a
// step may only start when all earlier steps are Succeeded.
func (j *Journal) StartStep(opID, stepID string, term uint64) error {
	return j.StartStepAs(opID, stepID, "", term)
}

// StartStepAs is StartStep that also records the agent step ID of the
// attempt ("" keeps the recorded one).
func (j *Journal) StartStepAs(opID, stepID, agentStep string, term uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, err := j.owned(opID, term)
	if err != nil {
		return err
	}
	if e.aborted {
		return fmt.Errorf("opjournal: operation %q is aborted", opID)
	}
	i, err := stepIndex(e, stepID)
	if err != nil {
		return err
	}
	for _, prev := range e.op.Steps[:i] {
		if prev.State != Succeeded {
			return fmt.Errorf("opjournal: step %q cannot start: earlier step %q is %s", stepID, prev.ID, prev.State)
		}
	}
	s := &e.op.Steps[i]
	switch s.State {
	case Pending, Unknown, Failed:
	default:
		return fmt.Errorf("opjournal: step %q cannot start from state %s", stepID, s.State)
	}
	s.State = Running
	s.Attempts++
	s.Retry = false
	if agentStep != "" {
		s.AgentStep = agentStep
	}
	return nil
}

// FailForRetry finishes a Running step as Failed and marks its retry as due
// (Step.Retry): the failure had no effect and the step's retry budget is not
// used up. Lab E4 (cleanuprestart): a remove-volume step that Docker refused
// ("volume in use") was in its retry backoff when the manager was killed;
// the new leader saw only Failed and failed the deletion.
func (j *Journal) FailForRetry(opID, stepID string, term uint64) error {
	if err := j.FinishStep(opID, stepID, Failed, term); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e := j.ops[opID]
	i, _ := stepIndex(e, stepID)
	e.op.Steps[i].Retry = true
	return nil
}

// FinishStep sets Succeeded, Failed or Unknown on a Running step. It returns
// ErrNotOwner if term != Owner.
func (j *Journal) FinishStep(opID, stepID string, result State, term uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, err := j.owned(opID, term)
	if err != nil {
		return err
	}
	switch result {
	case Succeeded, Failed, Unknown:
	default:
		return fmt.Errorf("opjournal: invalid step result %q", result)
	}
	i, err := stepIndex(e, stepID)
	if err != nil {
		return err
	}
	s := &e.op.Steps[i]
	if s.State != Running {
		return fmt.Errorf("opjournal: step %q is %s, not running", stepID, s.State)
	}
	s.State = result
	return nil
}

// Adopt hands the operation to a newer term and turns every Running step into
// Unknown. newTerm < Owner returns ErrNotOwner. newTerm == Owner is a
// successful no-op, so a replayed Adopt does not reset steps the new owner has
// started since.
func (j *Journal) Adopt(opID string, newTerm uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.ops[opID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, opID)
	}
	if newTerm < e.op.Owner {
		return fmt.Errorf("%w: got %d, owner %d", ErrNotOwner, newTerm, e.op.Owner)
	}
	if newTerm == e.op.Owner {
		return nil
	}
	e.op.Owner = newTerm
	for i := range e.op.Steps {
		if e.op.Steps[i].State == Running {
			e.op.Steps[i].State = Unknown
		}
	}
	return nil
}

// Abort ends the operation. It is refused while a step is Running; repeating
// it on an aborted operation is a no-op.
func (j *Journal) Abort(opID string, term uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, err := j.owned(opID, term)
	if err != nil {
		return err
	}
	for _, s := range e.op.Steps {
		if s.State == Running {
			return fmt.Errorf("opjournal: cannot abort %q: step %q is running", opID, s.ID)
		}
	}
	e.aborted = true
	return nil
}

// Get returns a deep copy of the operation.
func (j *Journal) Get(opID string) (Operation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.ops[opID]
	if !ok {
		return Operation{}, fmt.Errorf("%w: %q", ErrNotFound, opID)
	}
	return clone(e.op), nil
}

// Unfinished returns copies of all operations that are neither aborted nor
// fully Succeeded, sorted by ID.
func (j *Journal) Unfinished() []Operation {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Operation
	for _, e := range j.ops {
		if e.aborted {
			continue
		}
		done := true
		for _, s := range e.op.Steps {
			if s.State != Succeeded {
				done = false
				break
			}
		}
		if !done {
			out = append(out, clone(e.op))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}
