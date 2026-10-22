// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"errors"
	"testing"
)

type policyStore struct {
	fail error
	last *PolicyState
}

func (s *policyStore) PutPolicy(st PolicyState) error {
	if s.fail != nil {
		return s.fail
	}
	s.last = &st
	return nil
}

func TestPolicyPersistAndLoad(t *testing.T) {
	p := NewPolicy()
	store := &policyStore{}
	p.SetPersister(store)
	alice := Subject{ID: "alice", Kind: SubjectUser}
	admin := Binding{Subject: alice, Role: RoleAdministrator, Scope: Global}
	if err := p.Bind(admin); err != nil {
		t.Fatal(err)
	}
	custom := Role{Name: "ops", Permissions: []Permission{{Type: ResourceCluster, Action: ActionRead}}}
	if err := p.AddRole(custom); err != nil {
		t.Fatal(err)
	}
	if store.last == nil || len(store.last.Bindings) != 1 || len(store.last.Roles) != 1 {
		t.Fatalf("persisted %+v", store.last)
	}
	q := NewPolicy()
	if err := q.Load(*store.last); err != nil {
		t.Fatal(err)
	}
	if !q.Check(alice, ActionRead, Resource{Type: ResourceCluster, Scope: ClusterScope("s", "c")}).Allowed {
		t.Fatal("loaded policy lost the administrator binding")
	}
	if _, ok := q.Role("ops"); !ok {
		t.Fatal("custom role not loaded")
	}

	store.fail = errors.New("no quorum")
	bob := Binding{Subject: Subject{ID: "bob", Kind: SubjectUser}, Role: RoleAdministrator, Scope: Global}
	if err := p.Bind(bob); err == nil {
		t.Fatal("bind succeeded without persistence")
	}
	if len(p.Bindings(bob.Subject)) != 0 {
		t.Fatal("rejected binding visible")
	}
	if _, err := p.RemoveRole("ops"); err == nil {
		t.Fatal("remove succeeded without persistence")
	}
	if _, ok := p.Role("ops"); !ok {
		t.Fatal("rejected removal applied")
	}
	if err := p.Unbind(admin); err == nil || len(p.Bindings(alice)) != 1 {
		t.Fatal("rejected unbind applied")
	}
}
