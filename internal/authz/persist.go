// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"fmt"
	"slices"
	"strings"
)

// PolicyState is the persistable part of a Policy: custom roles and all
// bindings. Preset roles are shipped with the code and not persisted.
type PolicyState struct {
	Roles    []Role    `json:"roles"`
	Bindings []Binding `json:"bindings"`
}

// PolicyPersister stores the policy state. It is called synchronously with
// the policy lock held, after a change was validated and before it becomes
// visible; an error rejects the change. It must not call back into the
// Policy.
type PolicyPersister interface {
	PutPolicy(PolicyState) error
}

// SetPersister installs p (nil removes it).
func (p *Policy) SetPersister(pp PolicyPersister) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persist = pp
}

func (p *Policy) stateLocked() PolicyState {
	st := PolicyState{Roles: []Role{}, Bindings: slices.Clone(p.bindings)}
	if st.Bindings == nil {
		st.Bindings = []Binding{}
	}
	for _, r := range p.roles {
		if !r.Preset {
			st.Roles = append(st.Roles, cloneRole(r))
		}
	}
	slices.SortFunc(st.Roles, func(a, b Role) int { return strings.Compare(a.Name, b.Name) })
	return st
}

// State returns the current persistable state.
func (p *Policy) State() PolicyState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stateLocked()
}

// commitLocked persists the state after a change. On error it restores the
// previous roles and bindings and returns the error.
func (p *Policy) commitLocked(prevRoles map[string]Role, prevBindings []Binding) error {
	if p.persist == nil {
		return nil
	}
	if err := p.persist.PutPolicy(p.stateLocked()); err != nil {
		p.roles, p.bindings = prevRoles, prevBindings
		return fmt.Errorf("authz: persist policy: %w", err)
	}
	return nil
}

func (p *Policy) saveLocked() (map[string]Role, []Binding) {
	roles := make(map[string]Role, len(p.roles))
	for k, r := range p.roles {
		roles[k] = cloneRole(r)
	}
	return roles, slices.Clone(p.bindings)
}

// Load replaces custom roles and bindings with st (preset roles stay). It
// validates like AddRole and Bind; the persister is not called.
func (p *Policy) Load(st PolicyState) error {
	fresh := NewPolicy()
	for _, r := range st.Roles {
		if err := fresh.AddRole(r); err != nil {
			return err
		}
	}
	for _, b := range st.Bindings {
		if err := fresh.Bind(b); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roles, p.bindings = fresh.roles, fresh.bindings
	return nil
}
