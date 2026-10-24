// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var (
	alice  = Subject{ID: "alice", Kind: SubjectUser}
	bob    = Subject{ID: "bob", Kind: SubjectUser}
	admin  = Subject{ID: "root", Kind: SubjectUser}
	system = Subject{ID: "manager", Kind: SubjectSystem}
)

func db(swarm, cluster, name string) Resource {
	return Resource{Type: ResourceDatabase, Scope: DatabaseScope(swarm, cluster, name)}
}

func mustBind(t *testing.T, p *Policy, s Subject, role string, sc Scope) {
	t.Helper()
	if err := p.Bind(Binding{Subject: s, Role: role, Scope: sc}); err != nil {
		t.Fatalf("bind %s %s %s: %v", s.ID, role, sc, err)
	}
}

func TestDefaultDeny(t *testing.T) {
	p := NewPolicy()
	d := p.Check(alice, ActionRead, db("s1", "c1", "shop"))
	if d.Allowed || d.Reason != ReasonNoGrant {
		t.Fatalf("unbound subject: got %+v", d)
	}
	var zero Decision
	if zero.Allowed {
		t.Fatal("zero Decision must deny")
	}
	mustBind(t, p, alice, RoleViewer, Global)
	if d := p.Check(alice, ActionDelete, db("s1", "c1", "shop")); d.Allowed {
		t.Fatalf("viewer must not delete: %+v", d)
	}
	if d := p.Check(alice, ActionRead, Resource{Type: ResourceUserAccount, Scope: Global}); d.Allowed {
		t.Fatalf("viewer must not read accounts: %+v", d)
	}
	if d := p.Check(bob, ActionRead, db("s1", "c1", "shop")); d.Allowed {
		t.Fatalf("other subject must be denied: %+v", d)
	}
	// Same ID, different kind is a different subject.
	if d := p.Check(Subject{ID: "alice", Kind: SubjectAutomation}, ActionRead, db("s1", "c1", "shop")); d.Allowed {
		t.Fatalf("kind must be part of identity: %+v", d)
	}
}

func TestInvalidRequestsDeny(t *testing.T) {
	p := NewPolicy()
	mustBind(t, p, admin, RoleAdministrator, Global)
	cases := []struct {
		name string
		s    Subject
		a    Action
		r    Resource
	}{
		{"wildcard action", admin, ActionAny, db("s", "c", "d")},
		{"unknown action", admin, Action("drop"), db("s", "c", "d")},
		{"wildcard type", admin, ActionRead, Resource{Type: ResourceAny}},
		{"unknown type", admin, ActionRead, Resource{Type: "table"}},
		{"scope gap", admin, ActionRead, Resource{Type: ResourceDatabase, Scope: Scope{Database: "d"}}},
		{"empty subject", Subject{Kind: SubjectUser}, ActionRead, db("s", "c", "d")},
	}
	for _, tc := range cases {
		if d := p.Check(tc.s, tc.a, tc.r); d.Allowed || d.Reason != ReasonInvalidRequest {
			t.Errorf("%s: got %+v", tc.name, d)
		}
	}
}

func TestScopeInheritance(t *testing.T) {
	p := NewPolicy()
	mustBind(t, p, alice, RoleDatabaseAdmin, ClusterScope("s1", "c1"))

	allow := []Resource{
		db("s1", "c1", "shop"),
		db("s1", "c1", "blog"),
		{Type: ResourceDatabaseUser, Scope: DatabaseScope("s1", "c1", "shop"), ID: "app"},
		{Type: ResourceCluster, Scope: ClusterScope("s1", "c1")},
	}
	for _, r := range allow {
		a := ActionDelete
		if r.Type == ResourceCluster {
			a = ActionRead
		}
		if d := p.Check(alice, a, r); !d.Allowed || d.Scope != ClusterScope("s1", "c1") {
			t.Errorf("%s %s: want allowed via cluster binding, got %+v", a, r, d)
		}
	}
	deny := []Resource{
		db("s1", "c2", "shop"),                            // sibling cluster
		db("s2", "c1", "shop"),                            // same cluster ID in another Swarm
		{Type: ResourceCluster, Scope: SwarmScope("s1")},  // parent level
		{Type: ResourceSwarm, Scope: SwarmScope("s1")},    // parent resource
		{Type: ResourceDatabase, Scope: SwarmScope("s1")}, // listing at Swarm level
	}
	for _, r := range deny {
		if d := p.Check(alice, ActionRead, r); d.Allowed {
			t.Errorf("read %s: want denied, got %+v", r, d)
		}
	}

	// Database-level binding does not cover sibling databases or the cluster.
	mustBind(t, p, bob, RoleViewer, DatabaseScope("s1", "c1", "shop"))
	if d := p.Check(bob, ActionRead, db("s1", "c1", "shop")); !d.Allowed {
		t.Errorf("bob read shop: %+v", d)
	}
	if d := p.Check(bob, ActionRead, db("s1", "c1", "blog")); d.Allowed {
		t.Errorf("bob read sibling blog: %+v", d)
	}
	if d := p.Check(bob, ActionRead, Resource{Type: ResourceCluster, Scope: ClusterScope("s1", "c1")}); d.Allowed {
		t.Errorf("bob read parent cluster: %+v", d)
	}
}

func TestScopeCovers(t *testing.T) {
	cases := []struct {
		a, b Scope
		want bool
	}{
		{Global, DatabaseScope("s", "c", "d"), true},
		{SwarmScope("s"), ClusterScope("s", "c"), true},
		{SwarmScope("s"), SwarmScope("t"), false},
		{ClusterScope("s", "c"), DatabaseScope("s", "c", "d"), true},
		{ClusterScope("s", "c"), ClusterScope("s", "x"), false},
		{DatabaseScope("s", "c", "d"), DatabaseScope("s", "c", "d"), true},
		{DatabaseScope("s", "c", "d"), ClusterScope("s", "c"), false},
		{ClusterScope("s", "c"), Global, false},
	}
	for _, tc := range cases {
		if got := tc.a.Covers(tc.b); got != tc.want {
			t.Errorf("%s covers %s = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestEffectivePermissions(t *testing.T) {
	p := NewPolicy()
	if err := p.AddRole(Role{Name: "backup-only", Permissions: []Permission{
		{Type: ResourceBackup, Action: ActionBackup},
		{Type: ResourceBackup, Action: ActionRead},
	}}); err != nil {
		t.Fatal(err)
	}
	mustBind(t, p, alice, "backup-only", ClusterScope("s1", "c1"))
	mustBind(t, p, alice, "backup-only", ClusterScope("s1", "c2"))
	got := p.EffectivePermissions(alice)
	if len(got) != 4 {
		t.Fatalf("want 4 grants, got %d: %+v", len(got), got)
	}
	if got[0].Scope != ClusterScope("s1", "c1") || got[0].Permission.Action != ActionBackup {
		t.Errorf("unexpected ordering: %+v", got)
	}
	if len(p.EffectivePermissions(bob)) != 0 {
		t.Error("bob must have no grants")
	}
}

func TestRoleRemoval(t *testing.T) {
	p := NewPolicy()
	custom := Role{Name: "shop-dba", Permissions: []Permission{{Type: ResourceDatabase, Action: ActionAny}}}
	if err := p.AddRole(custom); err != nil {
		t.Fatal(err)
	}
	if err := p.AddRole(custom); !errors.Is(err, ErrRoleExists) {
		t.Fatalf("duplicate role: %v", err)
	}
	mustBind(t, p, alice, "shop-dba", DatabaseScope("s1", "c1", "shop"))
	mustBind(t, p, bob, "shop-dba", Global)
	mustBind(t, p, bob, RoleViewer, Global)
	if !p.Check(alice, ActionUpdate, db("s1", "c1", "shop")).Allowed {
		t.Fatal("alice should update shop before removal")
	}
	removed, err := p.RemoveRole("shop-dba")
	if err != nil || len(removed) != 2 {
		t.Fatalf("remove: %v, removed %d", err, len(removed))
	}
	if d := p.Check(alice, ActionUpdate, db("s1", "c1", "shop")); d.Allowed {
		t.Fatalf("alice keeps access after role removal: %+v", d)
	}
	if d := p.Check(bob, ActionUpdate, db("s1", "c1", "shop")); d.Allowed {
		t.Fatalf("bob keeps update after role removal: %+v", d)
	}
	if !p.Check(bob, ActionRead, db("s1", "c1", "shop")).Allowed {
		t.Fatal("unrelated viewer binding must survive")
	}
	// Re-adding a role with the same name must not resurrect old bindings.
	if err := p.AddRole(custom); err != nil {
		t.Fatal(err)
	}
	if p.Check(alice, ActionUpdate, db("s1", "c1", "shop")).Allowed {
		t.Fatal("old binding resurrected")
	}
	if _, err := p.RemoveRole(RoleAdministrator); !errors.Is(err, ErrPresetImmutable) {
		t.Fatalf("remove preset: %v", err)
	}
	if _, err := p.RemoveRole("nope"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("remove unknown: %v", err)
	}
	// Unbinding removes exactly one binding.
	if err := p.Unbind(Binding{Subject: bob, Role: RoleViewer, Scope: Global}); err != nil {
		t.Fatal(err)
	}
	if p.Check(bob, ActionRead, db("s1", "c1", "shop")).Allowed {
		t.Fatal("bob keeps read after unbind")
	}
	if err := p.Unbind(Binding{Subject: bob, Role: RoleViewer, Scope: Global}); !errors.Is(err, ErrUnknownBinding) {
		t.Fatalf("second unbind: %v", err)
	}
}

func TestProtectedResourceRule(t *testing.T) {
	p := NewPolicy()
	mustBind(t, p, admin, RoleAdministrator, Global)
	explicit := Role{Name: "explicit-delete", Permissions: []Permission{
		{Type: ResourceDatabaseUser, Action: ActionDelete},
		{Type: ResourceDatabaseUser, Action: ActionUpdate},
		{Type: ResourceDatabaseUser, Action: ActionRead},
	}}
	if err := p.AddRole(explicit); err != nil {
		t.Fatal(err)
	}
	mustBind(t, p, alice, "explicit-delete", Global)
	mustBind(t, p, system, RoleManagerSystem, Global)

	replUser := Resource{
		Type: ResourceDatabaseUser, Scope: DatabaseScope("s1", "c1", "mysql"),
		ID: "dbcm_replication", Protected: true,
	}
	for _, s := range []Subject{admin, alice} {
		for _, a := range []Action{ActionDelete, ActionUpdate} {
			d := p.Check(s, a, replUser)
			if d.Allowed || d.Reason != ReasonProtectedResource {
				t.Errorf("%s %s protected: got %+v", s.ID, a, d)
			}
		}
		if d := p.Check(s, ActionRead, replUser); !d.Allowed {
			t.Errorf("%s read protected must stay allowed: %+v", s.ID, d)
		}
	}
	if d := p.Check(admin, ActionRestore, Resource{Type: ResourceDatabase, Scope: DatabaseScope("s1", "c1", "dbcm"), Protected: true}); d.Allowed {
		t.Errorf("admin restore over protected: %+v", d)
	}
	// The same non-protected resource remains deletable.
	plain := replUser
	plain.Protected = false
	plain.ID = "app"
	if d := p.Check(alice, ActionDelete, plain); !d.Allowed {
		t.Errorf("delete plain user: %+v", d)
	}
	// Only the system role passes.
	if d := p.Check(system, ActionDelete, replUser); !d.Allowed || d.Role != RoleManagerSystem {
		t.Errorf("system delete protected: %+v", d)
	}

	// System roles cannot be created, bound to users, or modified.
	if err := p.AddRole(Role{Name: "fake-system", System: true, Permissions: []Permission{{Type: ResourceAny, Action: ActionAny}}}); !errors.Is(err, ErrSystemRole) {
		t.Errorf("custom system role: %v", err)
	}
	if err := p.AddRole(Role{Name: "fake-preset", Preset: true}); !errors.Is(err, ErrPresetImmutable) {
		t.Errorf("custom preset role: %v", err)
	}
	if err := p.Bind(Binding{Subject: alice, Role: RoleManagerSystem, Scope: Global}); !errors.Is(err, ErrSystemRole) {
		t.Errorf("bind system role to user: %v", err)
	}
	if err := p.Bind(Binding{Subject: Subject{ID: "rule1", Kind: SubjectAutomation}, Role: RoleManagerSystem, Scope: Global}); !errors.Is(err, ErrSystemRole) {
		t.Errorf("bind system role to automation: %v", err)
	}
	// A copy of the role returned by Role must not alias policy state.
	r, _ := p.Role(RoleAdministrator)
	r.System = true
	r.Permissions[0].Action = ActionRead
	if d := p.Check(admin, ActionDelete, replUser); d.Allowed {
		t.Error("mutating a returned role changed policy")
	}
	if d := p.Check(admin, ActionDelete, plain); !d.Allowed {
		t.Error("mutating a returned role changed policy permissions")
	}
}

func TestFilteredListingAndDashboardSharing(t *testing.T) {
	p := NewPolicy()
	// A role that only lets the holder view one shared dashboard.
	if err := p.AddRole(Role{Name: "dash-ops-viewer", Permissions: []Permission{
		{Type: ResourceDashboard, Action: ActionRead, ID: "ops"},
	}}); err != nil {
		t.Fatal(err)
	}
	mustBind(t, p, alice, "dash-ops-viewer", Global)
	mustBind(t, p, alice, RoleViewer, ClusterScope("s1", "c1"))

	type widget struct {
		Title  string
		Source Resource
	}
	widgets := []widget{
		{"shop qps", db("s1", "c1", "shop")},
		{"billing qps", db("s1", "c2", "billing")},
		{"c1 health", Resource{Type: ResourceCluster, Scope: ClusterScope("s1", "c1")}},
		{"hosts", Resource{Type: ResourceHost, Scope: SwarmScope("s1"), ID: "h1"}},
		{"other swarm", db("s2", "c1", "shop")},
	}
	dash := Resource{Type: ResourceDashboard, Scope: Global, ID: "ops"}
	if !p.Check(alice, ActionRead, dash).Allowed {
		t.Fatal("alice must see the shared dashboard")
	}
	if p.Check(alice, ActionRead, Resource{Type: ResourceDashboard, Scope: Global, ID: "finance"}).Allowed {
		t.Fatal("ID-restricted permission must not cover other dashboards")
	}
	got := Filter(p, alice, ActionRead, widgets, func(w widget) Resource { return w.Source })
	var titles []string
	for _, w := range got {
		titles = append(titles, w.Title)
	}
	if fmt.Sprint(titles) != "[shop qps c1 health]" {
		t.Fatalf("filtered widgets: %v", titles)
	}
	// Sharing the dashboard grants nothing on the underlying resources.
	mustBind(t, p, bob, "dash-ops-viewer", Global)
	if d := p.Check(bob, ActionRead, dash); !d.Allowed {
		t.Fatalf("bob dashboard: %+v", d)
	}
	if got := Filter(p, bob, ActionRead, widgets, func(w widget) Resource { return w.Source }); len(got) != 0 {
		t.Fatalf("bob sees %d widgets through sharing", len(got))
	}
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func TestAuthorizerAuditsStateChanges(t *testing.T) {
	p := NewPolicy()
	mustBind(t, p, alice, RoleOperator, ClusterScope("s1", "c1"))
	sink := &MemorySink{}
	clk := &fixedClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	a := NewAuthorizer(p, sink, clk.now)

	cl := Resource{Type: ResourceCluster, Scope: ClusterScope("s1", "c1")}
	if !a.Authorize(alice, ActionRead, cl).Allowed {
		t.Fatal("read")
	}
	if len(sink.Records()) != 0 {
		t.Fatal("reads must not be audited")
	}
	if !a.Authorize(alice, ActionOperate, cl).Allowed {
		t.Fatal("operate")
	}
	if a.Authorize(alice, ActionDelete, cl).Allowed {
		t.Fatal("delete must be denied")
	}
	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].Outcome != OutcomeAllowed || recs[0].Role != RoleOperator || !recs[0].Time.Equal(clk.t) || recs[0].Actor != alice {
		t.Errorf("allowed record: %+v", recs[0])
	}
	if recs[1].Outcome != OutcomeDenied || recs[1].Reason != ReasonNoGrant {
		t.Errorf("denied record: %+v", recs[1])
	}

	// A failing sink turns allowed state-changing decisions into denials.
	sink.SetErr(errors.New("disk full"))
	if d := a.Authorize(alice, ActionOperate, cl); d.Allowed || d.Reason != ReasonAuditUnavailable {
		t.Fatalf("fail closed: %+v", d)
	}
	if !a.Authorize(alice, ActionRead, cl).Allowed {
		t.Fatal("reads do not depend on the audit sink")
	}
}

func TestAuthorizerPolicyChanges(t *testing.T) {
	p := NewPolicy()
	mustBind(t, p, admin, RoleAdministrator, Global)
	mustBind(t, p, alice, RoleDatabaseAdmin, ClusterScope("s1", "c1"))
	if err := p.AddRole(Role{Name: "role-manager", Permissions: []Permission{
		{Type: ResourceRole, Action: ActionManagePermissions},
	}}); err != nil {
		t.Fatal(err)
	}
	mustBind(t, p, alice, "role-manager", ClusterScope("s1", "c1"))
	sink := &MemorySink{}
	a := NewAuthorizer(p, sink, nil)

	// alice may delegate her own rights inside her cluster.
	if err := a.Bind(alice, Binding{Subject: bob, Role: RoleViewer, Scope: DatabaseScope("s1", "c1", "shop")}); err == nil {
		t.Fatal("viewer includes swarm/host read that alice lacks; must be escalation")
	} else {
		var de *DeniedError
		if !errors.As(err, &de) || de.Decision.Reason != ReasonEscalation {
			t.Fatalf("want escalation, got %v", err)
		}
	}
	if err := a.Bind(alice, Binding{Subject: bob, Role: RoleDatabaseAdmin, Scope: DatabaseScope("s1", "c1", "shop")}); err != nil {
		t.Fatalf("delegate within scope: %v", err)
	}
	if !p.Check(bob, ActionDelete, db("s1", "c1", "shop")).Allowed {
		t.Fatal("delegated binding ineffective")
	}
	// Not outside her scope, and not the administrator role.
	if err := a.Bind(alice, Binding{Subject: bob, Role: RoleDatabaseAdmin, Scope: ClusterScope("s1", "c2")}); !errors.Is(err, ErrDenied) {
		t.Fatalf("bind outside scope: %v", err)
	}
	if err := a.Bind(alice, Binding{Subject: bob, Role: RoleAdministrator, Scope: ClusterScope("s1", "c1")}); !errors.Is(err, ErrDenied) {
		t.Fatalf("bind administrator: %v", err)
	}
	// Even an administrator cannot bind the system role.
	if err := a.Bind(admin, Binding{Subject: system, Role: RoleManagerSystem, Scope: Global}); !errors.Is(err, ErrDenied) {
		t.Fatalf("admin binds system role: %v", err)
	}
	// alice cannot create roles; admin can, and removing it is audited.
	if err := a.AddRole(alice, Role{Name: "x", Permissions: []Permission{{Type: ResourceDatabase, Action: ActionRead}}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("alice add role: %v", err)
	}
	if err := a.AddRole(admin, Role{Name: "x", Permissions: []Permission{{Type: ResourceDatabase, Action: ActionRead}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveRole(admin, RoleViewer); !errors.Is(err, ErrPresetImmutable) {
		t.Fatalf("remove preset via authorizer: %v", err)
	}
	if err := a.RemoveRole(admin, "x"); err != nil {
		t.Fatal(err)
	}
	if err := a.Unbind(alice, Binding{Subject: bob, Role: RoleDatabaseAdmin, Scope: DatabaseScope("s1", "c1", "shop")}); err != nil {
		t.Fatal(err)
	}

	var allowed, denied, ok, failed int
	for _, r := range sink.Records() {
		if r.Change == "" {
			t.Errorf("policy change record without description: %+v", r)
		}
		switch r.Outcome {
		case OutcomeAllowed:
			allowed++
		case OutcomeDenied:
			denied++
		case OutcomeSucceeded:
			ok++
		case OutcomeFailed:
			failed++
		}
	}
	// 10 attempts: 5 denied; 5 allowed of which 4 succeeded and 1 failed (preset).
	if denied != 5 || allowed != 5 || ok != 4 || failed != 1 {
		t.Fatalf("audit outcomes allowed=%d denied=%d succeeded=%d failed=%d", allowed, denied, ok, failed)
	}

	// A failing sink blocks policy changes before they are applied.
	sink.SetErr(errors.New("unavailable"))
	if err := a.Bind(admin, Binding{Subject: bob, Role: RoleViewer, Scope: Global}); !errors.Is(err, ErrDenied) {
		t.Fatalf("bind with failing sink: %v", err)
	}
	if len(p.Bindings(bob)) != 0 {
		t.Fatal("binding applied despite audit failure")
	}
}

func TestConcurrentChecks(t *testing.T) {
	p := NewPolicy()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		s := Subject{ID: fmt.Sprintf("u%d", i), Kind: SubjectUser}
		go func() {
			defer wg.Done()
			for j := range 50 {
				_ = p.Bind(Binding{Subject: s, Role: RoleViewer, Scope: DatabaseScope("s", "c", fmt.Sprint(j))})
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 50 {
				_ = p.Check(s, ActionRead, db("s", "c", fmt.Sprint(j)))
				_ = p.EffectivePermissions(s)
			}
		}()
	}
	wg.Wait()
	for i := range 8 {
		s := Subject{ID: fmt.Sprintf("u%d", i), Kind: SubjectUser}
		if !p.Check(s, ActionRead, db("s", "c", "49")).Allowed {
			t.Fatalf("u%d missing binding", i)
		}
	}
}

func TestVulnResourceDeniedByDefault(t *testing.T) {
	p := NewPolicy()
	vuln := Resource{Type: ResourceVuln, Scope: Global}
	for _, role := range []string{RoleViewer, RoleOperator, RoleDatabaseAdmin} {
		s := Subject{ID: "u-" + role, Kind: SubjectUser}
		mustBind(t, p, s, role, Global)
		for _, a := range []Action{ActionRead, ActionOperate, ActionUpdate} {
			if p.Check(s, a, vuln).Allowed {
				t.Errorf("%s may %s vuln", role, a)
			}
		}
	}
	mustBind(t, p, admin, RoleAdministrator, Global)
	if !p.Check(admin, ActionUpdate, vuln).Allowed {
		t.Fatal("administrator must hold vuln")
	}
}
