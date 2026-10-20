// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Warning codes.
const (
	WarnTOI          = "galera-toi"
	WarnLargeDDL     = "large-ddl"
	WarnDestructive  = "destructive"
	WarnUserExists   = "user-exists"
	WarnWildcardHost = "wildcard-host"
	WarnNotRepeat    = "not-repeatable"
	WarnNoop         = "noop"
	WarnRename       = "rename-incomplete"
)

// Warning is shown to the operator before a plan is applied.
type Warning struct {
	Code    string
	Message string
}

// Statement is one SQL statement of a plan. Display never contains secrets.
type Statement struct {
	Description string
	Display     string
	Idempotent  bool

	sql       string
	args      []any
	secret    *Secret
	secretRef SecretRef
}

// Plan lists what Apply would execute. Creating it changes nothing.
type Plan struct {
	WriterNode string
	Statements []Statement
	Warnings   []Warning
}

// Idempotent reports whether the whole plan can safely be applied again.
func (p *Plan) Idempotent() bool {
	for _, s := range p.Statements {
		if !s.Idempotent {
			return false
		}
	}
	return true
}

// Empty reports that there is nothing to do.
func (p *Plan) Empty() bool { return len(p.Statements) == 0 }

func (p *Plan) secrets() []string {
	var out []string
	for _, s := range p.Statements {
		if s.secret != nil {
			out = append(out, s.secret.Reveal())
		}
	}
	return out
}

func (p *Plan) warn(code, msg string) {
	for _, w := range p.Warnings {
		if w.Code == code && w.Message == msg {
			return
		}
	}
	p.Warnings = append(p.Warnings, Warning{Code: code, Message: msg})
}

const toiMessage = "DDL and account statements replicate via Total Order Isolation (TOI): they run on all nodes in the same order and block other DDL cluster-wide while they execute"

// mergePlans concatenates plans created for the same writer.
func mergePlans(plans ...*Plan) *Plan {
	out := &Plan{}
	for _, p := range plans {
		if out.WriterNode == "" {
			out.WriterNode = p.WriterNode
		}
		out.Statements = append(out.Statements, p.Statements...)
		for _, w := range p.Warnings {
			out.warn(w.Code, w.Message)
		}
	}
	return out
}

// Status of a statement after Apply.
type Status string

const (
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusPending Status = "pending" // not executed because an earlier statement failed
)

// StatementResult reports the outcome of one statement.
type StatementResult struct {
	Index       int
	Description string
	Status      Status
	Error       string // redacted
}

// ApplyResult describes what happened. On failure, earlier statements stay
// applied (there is no cross-statement transaction for DDL) and later ones are
// pending; re-applying the same Plan continues safely if it is idempotent.
type ApplyResult struct {
	Results []StatementResult
}

// Completed counts successfully executed statements.
func (r ApplyResult) Completed() int {
	n := 0
	for _, s := range r.Results {
		if s.Status == StatusDone {
			n++
		}
	}
	return n
}

// PartialFailureError is returned when a statement fails mid-plan.
type PartialFailureError struct {
	Index     int
	Completed int
	Total     int
	Err       error
}

func (e *PartialFailureError) Error() string {
	return fmt.Sprintf("dbadmin: statement %d of %d failed after %d completed: %v", e.Index+1, e.Total, e.Completed, e.Err)
}

func (e *PartialFailureError) Unwrap() error { return e.Err }

func redact(msg string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// Apply executes the plan statement by statement and stops at the first
// failure. It re-verifies that the executor is still on the writer chosen at
// planning time.
func (a *Admin) Apply(ctx context.Context, p *Plan) (ApplyResult, error) {
	res := ApplyResult{Results: make([]StatementResult, len(p.Statements))}
	for i, s := range p.Statements {
		res.Results[i] = StatementResult{Index: i, Description: s.Description, Status: StatusPending}
	}
	w, err := a.requireWriter(ctx)
	if err != nil {
		return res, err
	}
	if p.WriterNode != "" && w != p.WriterNode {
		return res, ErrWriterMoved
	}
	secrets := p.secrets()
	fail := func(i int, cause error) (ApplyResult, error) {
		if msg := cause.Error(); redact(msg, secrets) != msg {
			cause = errors.New(redact(msg, secrets))
		}
		res.Results[i].Status = StatusFailed
		res.Results[i].Error = redact(cause.Error(), secrets)
		return res, &PartialFailureError{Index: i, Completed: res.Completed(), Total: len(p.Statements), Err: cause}
	}
	for i, s := range p.Statements {
		if err := ctx.Err(); err != nil {
			return fail(i, err)
		}
		if s.secret != nil {
			if a.Sink == nil {
				return fail(i, ErrNoSink)
			}
			if err := a.Sink.Store(ctx, s.secretRef, *s.secret); err != nil {
				return fail(i, fmt.Errorf("storing generated secret: %w", err))
			}
		}
		if err := a.Exec.Exec(ctx, s.sql, s.args...); err != nil {
			return fail(i, err)
		}
		res.Results[i].Status = StatusDone
	}
	return res, nil
}
