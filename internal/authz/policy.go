// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Reason explains a decision.
type Reason string

const (
	ReasonGranted            Reason = "granted"
	ReasonNoGrant            Reason = "no_matching_grant"
	ReasonProtectedResource  Reason = "protected_resource"
	ReasonInvalidRequest     Reason = "invalid_request"
	ReasonAuditUnavailable   Reason = "audit_unavailable"
	ReasonEscalation         Reason = "privilege_escalation"
	ReasonSystemRoleRequired Reason = "system_role_required"
)

// Decision is the result of a check. The zero value denies.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  Reason `json:"reason"`
	// Detail is a human-readable explanation without secrets.
	Detail string `json:"detail,omitempty"`
	// Role and Scope name the binding that granted the request, if any.
	Role  string `json:"role,omitempty"`
	Scope Scope  `json:"scope"`
}

// Checker evaluates a request.
type Checker interface {
	Check(subject Subject, action Action, resource Resource) Decision
}

var (
	// ErrRoleExists is returned when a role name is already used.
	ErrRoleExists = errors.New("authz: role exists")
	// ErrUnknownRole is returned for a role that does not exist.
	ErrUnknownRole = errors.New("authz: unknown role")
	// ErrPresetImmutable is returned when changing or removing a preset role.
	ErrPresetImmutable = errors.New("authz: preset roles are immutable")
	// ErrSystemRole is returned when binding a system role to a non-system
	// subject or defining a system role outside the presets.
	ErrSystemRole = errors.New("authz: system roles are reserved for the manager")
	// ErrBindingExists is returned for a duplicate binding.
	ErrBindingExists = errors.New("authz: binding exists")
	// ErrUnknownBinding is returned when removing a missing binding.
	ErrUnknownBinding = errors.New("authz: unknown binding")
)

// Policy holds roles and bindings and evaluates checks. It is safe for
// concurrent use. Mutating methods do not authorize the caller; use
// Authorizer for actor-driven, audited changes. Direct calls are intended for
// bootstrap and for replaying replicated state.
type Policy struct {
	mu       sync.RWMutex
	roles    map[string]Role
	bindings []Binding
	persist  PolicyPersister
}

// NewPolicy returns a policy containing the preset roles.
func NewPolicy() *Policy {
	p := &Policy{roles: make(map[string]Role)}
	for _, r := range PresetRoles() {
		p.roles[r.Name] = cloneRole(r)
	}
	return p
}

func cloneRole(r Role) Role {
	r.Permissions = append([]Permission(nil), r.Permissions...)
	return r
}

// AddRole adds a custom role.
func (p *Policy) AddRole(r Role) error {
	if r.Name == "" {
		return errors.New("authz: empty role name")
	}
	if r.Preset {
		return ErrPresetImmutable
	}
	if r.System {
		return ErrSystemRole
	}
	for _, perm := range r.Permissions {
		if err := perm.Validate(); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.roles[r.Name]; ok {
		return fmt.Errorf("%w: %s", ErrRoleExists, r.Name)
	}
	pr, pb := p.saveLocked()
	p.roles[r.Name] = cloneRole(r)
	return p.commitLocked(pr, pb)
}

// Role returns a copy of the named role.
func (p *Policy) Role(name string) (Role, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.roles[name]
	if !ok {
		return Role{}, false
	}
	return cloneRole(r), true
}

// RemoveRole removes a custom role and every binding that references it. It
// returns the removed bindings.
func (p *Policy) RemoveRole(name string) ([]Binding, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.roles[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRole, name)
	}
	if r.Preset {
		return nil, ErrPresetImmutable
	}
	pr, pb := p.saveLocked()
	delete(p.roles, name)
	var kept, removed []Binding
	for _, b := range p.bindings {
		if b.Role == name {
			removed = append(removed, b)
		} else {
			kept = append(kept, b)
		}
	}
	p.bindings = kept
	if err := p.commitLocked(pr, pb); err != nil {
		return nil, err
	}
	return removed, nil
}

// Bind attaches a role to a subject at a scope.
func (p *Policy) Bind(b Binding) error {
	if err := b.Subject.Validate(); err != nil {
		return err
	}
	if err := b.Scope.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.roles[b.Role]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRole, b.Role)
	}
	if r.System != (b.Subject.Kind == SubjectSystem) {
		return ErrSystemRole
	}
	for _, x := range p.bindings {
		if x == b {
			return ErrBindingExists
		}
	}
	pr, pb := p.saveLocked()
	p.bindings = append(p.bindings, b)
	return p.commitLocked(pr, pb)
}

// Unbind removes a binding.
func (p *Policy) Unbind(b Binding) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, x := range p.bindings {
		if x == b {
			pr, pb := p.saveLocked()
			p.bindings = append(p.bindings[:i:i], p.bindings[i+1:]...)
			return p.commitLocked(pr, pb)
		}
	}
	return ErrUnknownBinding
}

// Bindings returns the subject's bindings.
func (p *Policy) Bindings(s Subject) []Binding {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Binding
	for _, b := range p.bindings {
		if b.Subject == s {
			out = append(out, b)
		}
	}
	return out
}

// EffectivePermissions returns every grant the subject holds through its
// bindings, sorted for stable output.
func (p *Policy) EffectivePermissions(s Subject) []Grant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Grant
	for _, b := range p.bindings {
		if b.Subject != s {
			continue
		}
		r, ok := p.roles[b.Role]
		if !ok {
			continue
		}
		for _, perm := range r.Permissions {
			out = append(out, Grant{Permission: perm, Scope: b.Scope, Role: r.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Scope.String() != b.Scope.String() {
			return a.Scope.String() < b.Scope.String()
		}
		if a.Permission.Type != b.Permission.Type {
			return a.Permission.Type < b.Permission.Type
		}
		if a.Permission.Action != b.Permission.Action {
			return a.Permission.Action < b.Permission.Action
		}
		if a.Permission.ID != b.Permission.ID {
			return a.Permission.ID < b.Permission.ID
		}
		return a.Role < b.Role
	})
	return out
}

func validateRequest(s Subject, a Action, r Resource) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !a.Valid() {
		return fmt.Errorf("authz: action %q is not a concrete known action", a)
	}
	if !r.Type.Valid() {
		return fmt.Errorf("authz: resource type %q is not a concrete known type", r.Type)
	}
	return r.Scope.Validate()
}

// Check evaluates a request. Everything not explicitly granted is denied.
// Destructive actions on protected resources are denied unless granted by a
// system role.
func (p *Policy) Check(s Subject, a Action, r Resource) Decision {
	if err := validateRequest(s, a, r); err != nil {
		return Decision{Reason: ReasonInvalidRequest, Detail: err.Error()}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	protectedHit := false
	for _, b := range p.bindings {
		if b.Subject != s || !b.Scope.Covers(r.Scope) {
			continue
		}
		role, ok := p.roles[b.Role]
		if !ok {
			continue
		}
		for _, perm := range role.Permissions {
			if !perm.matches(r.Type, a, r.ID) {
				continue
			}
			if r.Protected && a.Destructive() && !role.System {
				protectedHit = true
				continue
			}
			return Decision{
				Allowed: true,
				Reason:  ReasonGranted,
				Detail:  fmt.Sprintf("role %s at %s", role.Name, b.Scope),
				Role:    role.Name,
				Scope:   b.Scope,
			}
		}
	}
	if protectedHit {
		return Decision{
			Reason: ReasonProtectedResource,
			Detail: fmt.Sprintf("%s on %s is reserved for the manager", a, r),
		}
	}
	return Decision{
		Reason: ReasonNoGrant,
		Detail: fmt.Sprintf("no grant for %s on %s", a, r),
	}
}

// Holds reports whether the subject holds a grant that covers perm at scope.
// Wildcards in perm are only covered by equal or broader wildcards. It is
// used to prevent privilege escalation when binding roles.
func (p *Policy) Holds(s Subject, perm Permission, scope Scope) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, b := range p.bindings {
		if b.Subject != s || !b.Scope.Covers(scope) {
			continue
		}
		role, ok := p.roles[b.Role]
		if !ok {
			continue
		}
		for _, held := range role.Permissions {
			if held.covers(perm) {
				return true
			}
		}
	}
	return false
}
