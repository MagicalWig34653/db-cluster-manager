// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"sync"
	"time"
)

// Outcome is the result recorded in an audit record.
type Outcome string

const (
	// OutcomeAllowed and OutcomeDenied record an authorization decision for a
	// state-changing action. The caller records the operation's result
	// separately (for example in the operation journal).
	OutcomeAllowed Outcome = "allowed"
	OutcomeDenied  Outcome = "denied"
	// OutcomeSucceeded and OutcomeFailed record a policy change performed by
	// the Authorizer itself.
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

// AuditRecord describes one state-changing decision (U07): actor, time,
// target, change and result. It carries identifiers and descriptions only;
// callers must not put secrets into Change or Error.
type AuditRecord struct {
	Time     time.Time `json:"time"`
	Actor    Subject   `json:"actor"`
	Action   Action    `json:"action"`
	Resource Resource  `json:"resource"`
	Outcome  Outcome   `json:"outcome"`
	Reason   Reason    `json:"reason"`
	// Role and Scope name the granting binding of an allowed decision.
	Role  string `json:"role,omitempty"`
	Scope Scope  `json:"scope"`
	// Change describes a policy change, for example "bind role viewer".
	Change string `json:"change,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AuditSink stores audit records. An error makes the Authorizer fail closed.
type AuditSink interface {
	Record(AuditRecord) error
}

// MemorySink is an in-memory AuditSink for tests and bootstrapping.
type MemorySink struct {
	mu      sync.Mutex
	records []AuditRecord
	err     error
}

// SetErr makes subsequent Record calls fail with err (nil restores them).
func (m *MemorySink) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Record implements AuditSink.
func (m *MemorySink) Record(r AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, r)
	return nil
}

// Records returns a copy of the stored records.
func (m *MemorySink) Records() []AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRecord(nil), m.records...)
}
