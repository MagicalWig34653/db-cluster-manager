// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

func seedInventory(f *fixture) {
	f.inventory.swarms = []Swarm{
		{ID: "s1", Name: "one", State: StateHealthy},
		{ID: "s2", Name: "two", State: ""},
	}
	f.inventory.clusters = []Cluster{
		{ID: "c1", Swarm: "s1", Name: "orders", State: StateDegraded, Nodes: []ClusterNode{{ID: "n1", State: StateHealthy}, {ID: "n2"}}},
		{ID: "c2", Swarm: "s1", Name: "billing", State: "bogus"},
		{ID: "c3", Swarm: "s2", Name: "hidden", State: StateHealthy},
	}
	f.inventory.operations = []Operation{
		{ID: "o1", Kind: "restart", Swarm: "s1", Cluster: "c1", State: OperationRunning},
		{ID: "o2", Kind: "deploy-agent", Swarm: "s2", State: ""},
		{ID: "o3", Kind: "manager-upgrade", State: OperationSucceeded},
	}
	f.inventory.events = []Event{
		{ID: "e1", Swarm: "s1", Cluster: "c2", Severity: SeverityWarning, Message: "lag"},
		{ID: "e2", Swarm: "s2", Cluster: "c3", Severity: SeverityCritical, Message: "down"},
		{ID: "e3", Severity: "", Message: "manager"},
	}
}

func ids[T any](t *testing.T, r response, id func(T) string) []string {
	t.Helper()
	wantStatus(t, r, http.StatusOK)
	var l List[T]
	r.decode(t, &l)
	if l.Items == nil {
		t.Fatalf("items must be an array: %s", r.Body.String())
	}
	out := []string{}
	for _, it := range l.Items {
		out = append(out, id(it))
	}
	return out
}

func wantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestResourcesRequireSession(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/api/v1/swarms", "/api/v1/clusters", "/api/v1/clusters/c1", "/api/v1/operations", "/api/v1/events"} {
		wantError(t, f.client().do("GET", p, nil), http.StatusUnauthorized, errUnauthenticated)
	}
}

func TestAdministratorSeesEverything(t *testing.T) {
	f := newFixture(t)
	seedInventory(f)
	c, _, _ := f.setupAdmin("admin")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/swarms", nil), func(s Swarm) string { return s.ID }), "s1", "s2")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/clusters", nil), func(c Cluster) string { return c.ID }), "c1", "c2", "c3")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/operations", nil), func(o Operation) string { return o.ID }), "o1", "o2", "o3")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/events", nil), func(e Event) string { return e.ID }), "e1", "e2", "e3")
}

func TestListingsAreFilteredByPermission(t *testing.T) {
	f := newFixture(t)
	seedInventory(f)
	c := f.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	wantIDs(t, ids(t, c.do("GET", "/api/v1/swarms", nil), func(s Swarm) string { return s.ID }), "s1")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/clusters", nil), func(c Cluster) string { return c.ID }), "c1", "c2")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/operations", nil), func(o Operation) string { return o.ID }), "o1")
	wantIDs(t, ids(t, c.do("GET", "/api/v1/events", nil), func(e Event) string { return e.ID }), "e1")

	wantStatus(t, c.do("GET", "/api/v1/clusters/c1", nil), http.StatusOK)
	// Forbidden and missing objects look the same.
	wantError(t, c.do("GET", "/api/v1/clusters/c3", nil), http.StatusNotFound, errNotFound)
	wantError(t, c.do("GET", "/api/v1/clusters/nope", nil), http.StatusNotFound, errNotFound)

	// A cluster-scoped binding does not reveal the Swarm or sibling clusters.
	n := f.loggedIn("narrow", authz.RoleViewer, authz.ClusterScope("s1", "c2"))
	wantIDs(t, ids(t, n.do("GET", "/api/v1/swarms", nil), func(s Swarm) string { return s.ID }))
	wantIDs(t, ids(t, n.do("GET", "/api/v1/clusters", nil), func(c Cluster) string { return c.ID }), "c2")
	wantIDs(t, ids(t, n.do("GET", "/api/v1/events", nil), func(e Event) string { return e.ID }), "e1")
}

func TestAccountWithoutRoleSeesNothing(t *testing.T) {
	f := newFixture(t)
	seedInventory(f)
	c := f.loggedIn("nobody", "", authz.Global)
	wantIDs(t, ids(t, c.do("GET", "/api/v1/swarms", nil), func(s Swarm) string { return s.ID }))
	wantIDs(t, ids(t, c.do("GET", "/api/v1/clusters", nil), func(c Cluster) string { return c.ID }))
	wantError(t, c.do("GET", "/api/v1/clusters/c1", nil), http.StatusNotFound, errNotFound)
}

func TestUnknownStateIsNotFabricated(t *testing.T) {
	f := newFixture(t)
	seedInventory(f)
	c, _, _ := f.setupAdmin("admin")

	var sw List[Swarm]
	c.do("GET", "/api/v1/swarms", nil).decode(t, &sw)
	if sw.Items[0].State != StateHealthy || sw.Items[1].State != StateUnknown {
		t.Fatalf("swarms = %+v", sw.Items)
	}
	var cl Cluster
	r := c.do("GET", "/api/v1/clusters/c1", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &cl)
	if cl.State != StateDegraded || cl.Nodes[0].State != StateHealthy || cl.Nodes[1].State != StateUnknown {
		t.Fatalf("cluster = %+v", cl)
	}
	c.do("GET", "/api/v1/clusters/c2", nil).decode(t, &cl)
	if cl.State != StateUnknown || cl.Nodes == nil {
		t.Fatalf("bogus state must become unknown: %+v", cl)
	}
	var ops List[Operation]
	c.do("GET", "/api/v1/operations", nil).decode(t, &ops)
	if ops.Items[1].State != OperationUnknown {
		t.Fatalf("operations = %+v", ops.Items)
	}
	var evs List[Event]
	c.do("GET", "/api/v1/events", nil).decode(t, &evs)
	if evs.Items[2].Severity != SeverityUnknown {
		t.Fatalf("events = %+v", evs.Items)
	}
}

func TestServiceUnavailable(t *testing.T) {
	f := newFixture(t)
	c, _, _ := f.setupAdmin("admin")
	f.inventory.err = ErrUnavailable
	for _, p := range []string{"/api/v1/swarms", "/api/v1/clusters", "/api/v1/clusters/c1", "/api/v1/operations", "/api/v1/events"} {
		wantError(t, c.do("GET", p, nil), http.StatusServiceUnavailable, errUnavailable)
	}
	f.inventory.err = nil
	f.srv.cfg.Inventory = Unavailable{}
	f.srv.cfg.Activity = Unavailable{}
	f.handler = f.srv.mux()
	wantError(t, c.do("GET", "/api/v1/swarms", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, c.do("GET", "/api/v1/events", nil), http.StatusServiceUnavailable, errUnavailable)
}

// stateChangingEndpoint is a synthetic endpoint that exercises the
// permission middleware for a state-changing action; the real API has none
// yet.
func stateChangingEndpoint(called *bool) endpoint {
	return endpoint{
		method: "POST", path: "/test/clusters/{id}/restart", access: accessPermission, action: authz.ActionOperate,
		resolve: func(r *http.Request) (authz.Resource, any, error) {
			return authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope("s1", r.PathValue("id"))}, nil, nil
		},
		handle: func(w http.ResponseWriter, _ *http.Request, _ *caller) {
			*called = true
			w.WriteHeader(http.StatusAccepted)
		},
	}
}

func TestPermissionMiddlewareAuditsAndFailsClosed(t *testing.T) {
	f := newFixture(t)
	var called bool
	ep := stateChangingEndpoint(&called)
	op := f.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	mux := http.NewServeMux()
	mux.Handle(ep.method+" "+ep.path, f.srv.wrap(ep))
	f.handler = mux

	countAudit := func(actor string, out authz.Outcome) int {
		n := 0
		for _, r := range f.sink.Records() {
			if r.Actor.ID == actor && r.Action == authz.ActionOperate && r.Outcome == out {
				n++
			}
		}
		return n
	}

	wantStatus(t, op.do("POST", "/test/clusters/c1/restart", nil), http.StatusAccepted)
	if !called || countAudit("op", authz.OutcomeAllowed) != 1 {
		t.Fatalf("called=%v audit=%+v", called, f.sink.Records())
	}

	called = false
	wantError(t, viewer.do("POST", "/test/clusters/c1/restart", nil), http.StatusForbidden, errForbidden)
	if called || countAudit("view", authz.OutcomeDenied) != 1 {
		t.Fatal("denied request must be audited and not run")
	}

	f.sink.SetErr(errors.New("audit down"))
	wantError(t, op.do("POST", "/test/clusters/c1/restart", nil), http.StatusServiceUnavailable, errUnavailable)
	if called {
		t.Fatal("handler must not run when the audit sink fails")
	}
}

func TestEndpointDeclarationsAreEnforced(t *testing.T) {
	f := newFixture(t)
	noop := func(http.ResponseWriter, *http.Request, *caller) {}
	for name, ep := range map[string]endpoint{
		"no access":       {method: "GET", path: "/x", handle: noop},
		"no action":       {method: "GET", path: "/x", access: accessPermission, resolve: func(*http.Request) (authz.Resource, any, error) { return authz.Resource{}, nil, nil }, handle: noop},
		"no resource":     {method: "GET", path: "/x", access: accessPermission, action: authz.ActionRead, handle: noop},
		"wildcard action": {method: "GET", path: "/x", access: accessPermission, action: authz.ActionAny, resolve: func(*http.Request) (authz.Resource, any, error) { return authz.Resource{}, nil, nil }, handle: noop},
		"no handler":      {method: "GET", path: "/x", access: accessPublic},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f.srv.wrap(ep)
		})
	}
	for _, ep := range (*server)(nil).endpoints() {
		if ep.access == 0 {
			t.Fatalf("%s %s declares no access", ep.method, ep.path)
		}
	}
}

func TestPrototypeHandlerServesOnlyProcessEndpoints(t *testing.T) {
	h := NewHandler()
	for _, p := range []string{"/api/v1/setup/status", "/api/v1/swarms", "/api/v1/auth/session"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config must be rejected")
	}
}
