// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// Errors returned by the Runner.
var (
	// ErrNotLeader: this manager is not the metastore leader, or the barrier
	// failed. Nothing was journaled or sent.
	ErrNotLeader = errors.New("runner: not the metastore leader")
	// ErrLostLeadership: leadership or the operation's term was lost while
	// the operation ran. The runner stopped; a new leader adopts it.
	ErrLostLeadership = errors.New("runner: leadership lost")
	// ErrBusy: a lock key of the operation is held by another operation.
	ErrBusy = errors.New("runner: resource locked by another operation")
	// ErrInvalidOperation: the operation definition is malformed.
	ErrInvalidOperation = errors.New("runner: invalid operation")
	// ErrUnknownOperation: the operation is not registered with this runner.
	ErrUnknownOperation = errors.New("runner: unknown operation")
	// ErrActive: the operation is executing right now.
	ErrActive = errors.New("runner: operation is executing")
	// ErrNotPaused: Reconcile was called for an operation or step that is
	// not paused in doubt.
	ErrNotPaused = errors.New("runner: operation step is not paused")
	// ErrFinished: the operation already succeeded or was aborted.
	ErrFinished = errors.New("runner: operation is finished")
)

// Leader is the leadership port, implemented by *metastore.Node.
type Leader interface {
	IsLeader() bool
	Term() uint64
	Barrier(timeout time.Duration) error
}

// Journal is the replicated operation journal port. Writes must go through
// consensus (see MetastoreJournal); reads may be local.
type Journal interface {
	Begin(opID, kind string, stepIDs []string, term uint64) error
	StartStep(opID, stepID string, term uint64) error
	FinishStep(opID, stepID string, result opjournal.State, term uint64) error
	Adopt(opID string, term uint64) error
	Abort(opID string, term uint64) error
	Get(opID string) (opjournal.Operation, error)
	Unfinished() ([]opjournal.Operation, error)
}

// AgentClient delivers one command to the agent named by target. An error
// means the command was not answered: completion is unknown unless the error
// is a known rejection (fencing.ErrStaleTerm, agent.ErrInvalidCommand,
// agent.ErrStepConflict), which the agent returns without effect.
type AgentClient interface {
	Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error)
}

// Condition is a precondition of a step, as data so an operation can be
// rebuilt and compared by a new leader.
type Condition struct {
	Kind        string `json:"kind"`
	StepID      string `json:"step_id,omitempty"`
	Target      string `json:"target,omitempty"` // for example a node slot
	ClusterSize int    `json:"cluster_size,omitempty"`
}

// CondStepSucceeded is evaluated by the runner from the journal: the step
// StepID of the same operation has Succeeded.
const CondStepSucceeded = "step-succeeded"

// Checker evaluates conditions on observed live state. It returns ok=false
// with a reason when the condition does not hold. Without a Checker every
// condition other than CondStepSucceeded is unmet (fail closed).
type Checker interface {
	Check(ctx context.Context, opID string, step Step, c Condition) (ok bool, reason string, err error)
}

// Step is one ordered unit of an operation.
type Step struct {
	ID string `json:"id"`
	// Target is the agent (host) that receives Command.
	Target string `json:"target,omitempty"`
	// Command is the agent command kind. Empty makes the step a gate: it
	// sends nothing and succeeds once its preconditions hold.
	Command agent.Kind      `json:"command,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	// Preconditions must all hold before the step starts.
	Preconditions []Condition `json:"preconditions,omitempty"`
	// Timeout bounds one command attempt; zero uses Config.StepTimeout.
	Timeout time.Duration `json:"timeout,omitempty"`
	// MaxRetries bounds extra attempts of this step in one run: re-queries
	// of an unanswered command, automatic retries of idempotent kinds and
	// precondition polls.
	MaxRetries int    `json:"max_retries,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Operation is a long-running operation made of ordered steps.
type Operation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Locks are resource keys ("cluster/<id>", "node/<id>/<slot>"). Two
	// operations sharing a key never execute concurrently.
	Locks []string `json:"locks"`
	Steps []Step   `json:"steps"`
}

// reservedSep separates a step ID from the suffix of a new agent step ID.
const reservedSep = "~"

// Validate checks the shape of the operation.
func (op Operation) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOperation, fmt.Sprintf(format, a...))
	}
	if op.ID == "" || op.Kind == "" {
		return bad("ID and kind required")
	}
	if len(op.Steps) == 0 {
		return bad("operation %q has no steps", op.ID)
	}
	if len(op.Locks) == 0 {
		return bad("operation %q names no lock keys", op.ID)
	}
	seen := map[string]int{}
	for i, s := range op.Steps {
		if s.ID == "" || strings.Contains(s.ID, reservedSep) {
			return bad("step %d: ID must be non-empty and not contain %q", i, reservedSep)
		}
		if _, dup := seen[s.ID]; dup {
			return bad("duplicate step ID %q", s.ID)
		}
		seen[s.ID] = i
		if s.Command != "" && s.Target == "" {
			return bad("step %q: command without target", s.ID)
		}
		if s.MaxRetries < 0 || s.Timeout < 0 {
			return bad("step %q: negative retries or timeout", s.ID)
		}
		for _, c := range s.Preconditions {
			if c.Kind == CondStepSucceeded {
				j, ok := seen[c.StepID]
				if !ok || j >= i {
					return bad("step %q: condition on step %q that is not earlier", s.ID, c.StepID)
				}
			}
		}
	}
	return nil
}

func (op Operation) stepIDs() []string {
	ids := make([]string, len(op.Steps))
	for i, s := range op.Steps {
		ids[i] = s.ID
	}
	return ids
}

// Idempotent reports whether a command kind may be executed again under a
// new step ID without reconciling live state first. Every kind not listed
// changes state and is treated as destructive.
func Idempotent(k agent.Kind) bool {
	switch k {
	case agent.KindInspect, agent.KindListOwned, agent.KindVerifyIdentity, agent.KindAllowObjects:
		return true
	}
	return false
}

// Status is the runner-level state of an operation.
type Status string

// Operation statuses.
const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed" // a step failed with an observed result
	StatusPaused    Status = "paused" // needs Resume or Reconcile
	StatusAborted   Status = "aborted"
	StatusStopped   Status = "stopped" // this runner no longer owns it
)

// Resolution is the decision of a reconcile after the live state of an
// in-doubt step was inspected.
type Resolution string

// Resolutions.
const (
	// ResolveApplied: the effect is present; the step is recorded Succeeded
	// without sending anything.
	ResolveApplied Resolution = "applied"
	// ResolveNotApplied: the effect is absent; the step is sent again under
	// a new agent step ID.
	ResolveNotApplied Resolution = "not-applied"
	// ResolveFailed: the step is recorded Failed and the operation fails.
	ResolveFailed Resolution = "failed"
	// ResolveAbort: the operation is aborted.
	ResolveAbort Resolution = "abort"
)

// EventType classifies a timeline event.
type EventType string

// Timeline event types.
const (
	EventBegin          EventType = "begin"
	EventAdopt          EventType = "adopt"
	EventWait           EventType = "precondition-wait"
	EventStart          EventType = "step-start"
	EventSend           EventType = "send"
	EventQuery          EventType = "query-recorded-outcome"
	EventResult         EventType = "result"
	EventFinish         EventType = "step-finish"
	EventRetry          EventType = "retry"
	EventPause          EventType = "pause"
	EventReconcile      EventType = "reconcile"
	EventAbortRequested EventType = "abort-requested"
	EventAbort          EventType = "abort"
	EventSucceed        EventType = "succeed"
	EventFail           EventType = "fail"
	EventStop           EventType = "stop"
)

// Event is one entry of an operation's timeline.
type Event struct {
	At     time.Time `json:"at"`
	Term   uint64    `json:"term"`
	Type   EventType `json:"type"`
	StepID string    `json:"step_id,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// StepView is the observable state of one step.
type StepView struct {
	ID          string          `json:"id"`
	Target      string          `json:"target,omitempty"`
	Command     agent.Kind      `json:"command,omitempty"`
	State       opjournal.State `json:"state"`
	Attempts    int             `json:"attempts"`
	AgentStepID string          `json:"agent_step_id,omitempty"`
	LastResult  *agent.Result   `json:"last_result,omitempty"`
}

// View is a snapshot of an operation for the UI and the audit log.
type View struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Status         Status     `json:"status"`
	Term           uint64     `json:"term"`
	Reason         string     `json:"reason,omitempty"`
	PausedStep     string     `json:"paused_step,omitempty"`
	AbortRequested bool       `json:"abort_requested,omitempty"`
	Locks          []string   `json:"locks"`
	Steps          []StepView `json:"steps"`
	Timeline       []Event    `json:"timeline"`
}
