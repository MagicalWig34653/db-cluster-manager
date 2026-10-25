// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"errors"
	"fmt"
	"time"
)

// ErrDenied wraps a denied Decision returned by Authorizer policy changes.
var ErrDenied = errors.New("authz: denied")

// DeniedError carries the decision that denied a policy change.
type DeniedError struct{ Decision Decision }

func (e *DeniedError) Error() string {
	return fmt.Sprintf("authz: denied (%s): %s", e.Decision.Reason, e.Decision.Detail)
}

// Unwrap lets errors.Is(err, ErrDenied) succeed.
func (e *DeniedError) Unwrap() error { return ErrDenied }

// Authorizer wraps a Policy with auditing. Every decision for a
// state-changing action and every policy change produces an audit record. If
// the audit sink fails, allowed decisions are turned into denials (fail
// closed) and policy changes are not applied.
type Authorizer struct {
	policy *Policy
	sink   AuditSink
	now    func() time.Time
}

// NewAuthorizer returns an Authorizer. now may be nil for time.Now.
func NewAuthorizer(p *Policy, sink AuditSink, now func() time.Time) *Authorizer {
	if now == nil {
		now = time.Now
	}
	return &Authorizer{policy: p, sink: sink, now: now}
}

// Policy returns the underlying policy for read-only use such as listing.
func (a *Authorizer) Policy() *Policy { return a.policy }

// Check evaluates a request without auditing. Use it only for actions that do
// not change state; Authorize audits state-changing actions.
func (a *Authorizer) Check(s Subject, act Action, r Resource) Decision {
	return a.policy.Check(s, act, r)
}

// Authorize evaluates a request and audits it when the action changes state.
func (a *Authorizer) Authorize(s Subject, act Action, r Resource) Decision {
	d := a.policy.Check(s, act, r)
	if !act.ChangesState() {
		return d
	}
	rec := a.record(s, act, r, d)
	if err := a.sink.Record(rec); err != nil && d.Allowed {
		return Decision{Reason: ReasonAuditUnavailable, Detail: "audit record could not be stored"}
	}
	return d
}

func (a *Authorizer) record(s Subject, act Action, r Resource, d Decision) AuditRecord {
	out := OutcomeDenied
	if d.Allowed {
		out = OutcomeAllowed
	}
	return AuditRecord{
		Time: a.now().UTC(), Actor: s, Action: act, Resource: r,
		Outcome: out, Reason: d.Reason, Role: d.Role, Scope: d.Scope,
	}
}

// change authorizes actor for act on r, audits the attempt and applies fn
// only if the decision allows it and the audit record of the attempt was
// stored. The result of fn is audited as well.
func (a *Authorizer) change(actor Subject, act Action, r Resource, desc string, extra func() Decision, fn func() error) error {
	d := a.policy.Check(actor, act, r)
	if d.Allowed && extra != nil {
		if x := extra(); !x.Allowed {
			d = x
		}
	}
	rec := a.record(actor, act, r, d)
	rec.Change = desc
	if err := a.sink.Record(rec); err != nil {
		return &DeniedError{Decision: Decision{Reason: ReasonAuditUnavailable, Detail: "audit record could not be stored"}}
	}
	if !d.Allowed {
		return &DeniedError{Decision: d}
	}
	err := fn()
	res := rec
	res.Outcome = OutcomeSucceeded
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Error = err.Error()
	}
	// The change is already applied; a failing sink cannot undo it, so the
	// error is reported to the caller alongside the operation result.
	if serr := a.sink.Record(res); serr != nil && err == nil {
		return fmt.Errorf("authz: change applied but result audit failed: %w", serr)
	}
	return err
}

func roleResource(name string) Resource {
	return Resource{Type: ResourceRole, Scope: Global, ID: name}
}

// AddRole creates a custom role. The actor needs create on roles and must
// itself hold every permission of the new role globally, so a role cannot be
// used to define rights the actor lacks.
func (a *Authorizer) AddRole(actor Subject, r Role) error {
	return a.change(actor, ActionCreate, roleResource(r.Name), "add role "+r.Name,
		func() Decision { return a.escalation(actor, r.Permissions, Global) },
		func() error { return a.policy.AddRole(r) })
}

// RemoveRole removes a custom role and its bindings.
func (a *Authorizer) RemoveRole(actor Subject, name string) error {
	return a.change(actor, ActionDelete, roleResource(name), "remove role "+name, nil,
		func() error {
			_, err := a.policy.RemoveRole(name)
			return err
		})
}

// Bind attaches a role to a subject. The actor needs manage_permissions on
// roles at the binding scope and must hold every permission of the role at
// that scope.
func (a *Authorizer) Bind(actor Subject, b Binding) error {
	r := Resource{Type: ResourceRole, Scope: b.Scope, ID: b.Role}
	return a.change(actor, ActionManagePermissions, r,
		fmt.Sprintf("bind role %s to %s:%s at %s", b.Role, b.Subject.Kind, b.Subject.ID, b.Scope),
		func() Decision { return a.bindEscalation(actor, b) },
		func() error { return a.policy.Bind(b) })
}

// Unbind removes a binding. The actor needs manage_permissions on roles at
// the binding scope.
func (a *Authorizer) Unbind(actor Subject, b Binding) error {
	r := Resource{Type: ResourceRole, Scope: b.Scope, ID: b.Role}
	return a.change(actor, ActionManagePermissions, r,
		fmt.Sprintf("unbind role %s from %s:%s at %s", b.Role, b.Subject.Kind, b.Subject.ID, b.Scope),
		nil, func() error { return a.policy.Unbind(b) })
}

func (a *Authorizer) bindEscalation(actor Subject, b Binding) Decision {
	role, ok := a.policy.Role(b.Role)
	if !ok {
		// Let Policy.Bind report the unknown role as the change result.
		return Decision{Allowed: true}
	}
	if role.System {
		return Decision{Reason: ReasonSystemRoleRequired, Detail: "system roles cannot be bound through the authorizer"}
	}
	return a.escalation(actor, role.Permissions, b.Scope)
}

func (a *Authorizer) escalation(actor Subject, perms []Permission, scope Scope) Decision {
	for _, p := range perms {
		if !a.policy.Holds(actor, p, scope) {
			return Decision{
				Reason: ReasonEscalation,
				Detail: fmt.Sprintf("actor lacks %s on %s at %s", p.Action, p.Type, scope),
			}
		}
	}
	return Decision{Allowed: true}
}

// AuthorizeAdministrator restricts key and membership administration to the
// global administrator preset, including when a custom role has wildcard grants.
// Both allowed and denied attempts must reach the audit sink.
func (a *Authorizer) AuthorizeAdministrator(s Subject) error {
	r := Resource{Type: ResourceSettings, Scope: Global}
	d := Decision{Reason: ReasonNoGrant, Detail: "global administrator required"}
	for _, b := range a.policy.Bindings(s) {
		if s.Kind == SubjectUser && b.Role == RoleAdministrator && b.Scope == Global {
			d = Decision{Allowed: true, Reason: ReasonGranted, Role: RoleAdministrator, Scope: Global}
			break
		}
	}
	if err := a.sink.Record(a.record(s, ActionUpdate, r, d)); err != nil {
		return &DeniedError{Decision: Decision{Reason: ReasonAuditUnavailable}}
	}
	if !d.Allowed {
		return &DeniedError{Decision: d}
	}
	return nil
}
