// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

type fakeMonitoring struct {
	mu     sync.Mutex
	dash   map[string]metrics.Dashboard
	notes  []Notification
	acked  []string
	series []metrics.Labels
	err    error
	qerr   error
	lastQ  metrics.Query
}

func newFakeMonitoring() *fakeMonitoring {
	return &fakeMonitoring{dash: map[string]metrics.Dashboard{}}
}

func (f *fakeMonitoring) Dashboards(context.Context) ([]metrics.Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []metrics.Dashboard
	for _, d := range f.dash {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, f.err
}

func (f *fakeMonitoring) Dashboard(_ context.Context, id string) (metrics.Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return metrics.Dashboard{}, f.err
	}
	d, ok := f.dash[id]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}

func (f *fakeMonitoring) CreateDashboard(_ context.Context, d metrics.Dashboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.dash[d.ID]; ok {
		return ErrConflict
	}
	f.dash[d.ID] = d
	return nil
}

func (f *fakeMonitoring) UpdateDashboard(_ context.Context, d metrics.Dashboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dash[d.ID] = d
	return nil
}

func (f *fakeMonitoring) DeleteDashboard(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dash, id)
	return nil
}

func (f *fakeMonitoring) Query(_ context.Context, q metrics.Query) (*metrics.Result, error) {
	f.mu.Lock()
	f.lastQ = q
	f.mu.Unlock()
	if f.qerr != nil {
		return nil, f.qerr
	}
	res := &metrics.Result{Start: q.Start, End: q.End, Step: q.Step, Window: q.Step}
	for _, l := range f.series {
		if !q.Selector.Matches(l) || !q.Authorizer(l) {
			continue
		}
		res.Series = append(res.Series, metrics.SeriesResult{Labels: l, Points: []metrics.Point{
			{T: q.Start, V: 3, State: metrics.StateOK},
			{T: q.Start + q.Step, V: math.NaN(), State: metrics.StateUnavailable},
		}})
	}
	res.Gaps = []metrics.Gap{{From: q.Start + q.Step, To: q.End, Reason: "expired"}}
	return res, nil
}

func (f *fakeMonitoring) Notifications(context.Context) ([]Notification, error) {
	return append([]Notification(nil), f.notes...), f.err
}

func (f *fakeMonitoring) Notification(_ context.Context, id string) (Notification, error) {
	for _, n := range f.notes {
		if n.ID == id {
			return n, nil
		}
	}
	return Notification{}, ErrNotFound
}

func (f *fakeMonitoring) AckNotification(_ context.Context, id, by string) error {
	f.acked = append(f.acked, id+"/"+by)
	return nil
}

var panel = map[string]any{"id": "p1", "title": "Size", "kind": "line", "selector": "dcm_galera_cluster_size", "func": "last",
	"stepSeconds": 15, "x": 0, "y": 0, "w": 6, "h": 4}

func dashBody(title string, shared bool) map[string]any {
	return map[string]any{"title": title, "shared": shared, "panels": []any{panel}}
}

func auditCount(f *fixture, act authz.Action, out authz.Outcome) int {
	n := 0
	for _, r := range f.sink.Records() {
		if r.Action == act && r.Outcome == out && (r.Resource.Type == authz.ResourceDashboard || r.Resource.Type == authz.ResourceSettings || r.Resource.Type == authz.ResourceCluster) {
			n++
		}
	}
	return n
}

func TestMonitoringEndpointsRequireSession(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.Monitoring = newFakeMonitoring()
	c := f.client()
	wantError(t, c.do("GET", "/api/v1/dashboards", nil), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, c.do("POST", "/api/v1/dashboards/x", dashBody("t", false)), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, c.do("POST", "/api/v1/metrics/query", map[string]any{}), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, c.do("GET", "/api/v1/notifications", nil), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, c.do("POST", "/api/v1/notifications/n1/ack", nil), http.StatusUnauthorized, errUnauthenticated)
}

func TestMonitoringUnavailableWithoutService(t *testing.T) {
	f := newFixture(t)
	c, _, _ := f.setupAdmin("admin")
	wantError(t, c.do("GET", "/api/v1/dashboards", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, c.do("GET", "/api/v1/notifications", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, c.do("POST", "/api/v1/metrics/query", map[string]any{"selector": "m", "start": 0, "end": 1, "stepSeconds": 1}), http.StatusServiceUnavailable, errUnavailable)
}

func TestDashboardLifecycleAuditedAndOwnerOnly(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	f.srv.cfg.Monitoring = fm
	admin, _, _ := f.setupAdmin("admin")

	r := admin.do("POST", "/api/v1/dashboards/d1", dashBody("Mine", false))
	wantStatus(t, r, http.StatusCreated)
	var d metrics.Dashboard
	r.decode(t, &d)
	if d.Owner != "admin" || d.ID != "d1" || d.Shared {
		t.Fatalf("%+v", d)
	}
	if auditCount(f, authz.ActionCreate, authz.OutcomeAllowed) != 1 {
		t.Fatal("create must be audited")
	}
	wantError(t, admin.do("POST", "/api/v1/dashboards/d1", dashBody("Again", false)), http.StatusConflict, errConflict)

	// invalid bodies and IDs
	bad := dashBody("x", false)
	bad["panels"] = []any{map[string]any{"id": "p", "title": "t", "kind": "line", "selector": "m{", "func": "last", "stepSeconds": 1, "x": 0, "y": 0, "w": 1, "h": 1}}
	wantError(t, admin.do("POST", "/api/v1/dashboards/d2", bad), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/dashboards/bad%20id", dashBody("x", false)), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/dashboards/d3", map[string]any{"title": "x", "owner": "evil", "panels": []any{}}), http.StatusBadRequest, errBadRequest)

	// update keeps the owner; sharing flag changes
	r = admin.do("PUT", "/api/v1/dashboards/d1", dashBody("Renamed", true))
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &d)
	if d.Title != "Renamed" || !d.Shared || d.Owner != "admin" {
		t.Fatalf("%+v", d)
	}
	if auditCount(f, authz.ActionUpdate, authz.OutcomeAllowed) != 1 {
		t.Fatal("update must be audited")
	}

	// another administrator can see the shared dashboard but not change it
	other := f.loggedIn("other", authz.RoleAdministrator, authz.Global)
	wantIDs(t, ids(t, other.do("GET", "/api/v1/dashboards", nil), func(d metrics.Dashboard) string { return d.ID }), "d1")
	wantError(t, other.do("PUT", "/api/v1/dashboards/d1", dashBody("Hijack", true)), http.StatusNotFound, errNotFound)
	wantError(t, other.do("DELETE", "/api/v1/dashboards/d1", nil), http.StatusNotFound, errNotFound)
	if fm.dash["d1"].Title != "Renamed" {
		t.Fatal("non-owner changed a dashboard")
	}

	// personal dashboards are invisible to others
	wantStatus(t, admin.do("PUT", "/api/v1/dashboards/d1", dashBody("Renamed", false)), http.StatusOK)
	wantIDs(t, ids(t, other.do("GET", "/api/v1/dashboards", nil), func(d metrics.Dashboard) string { return d.ID }))
	wantIDs(t, ids(t, admin.do("GET", "/api/v1/dashboards", nil), func(d metrics.Dashboard) string { return d.ID }), "d1")

	wantStatus(t, admin.do("DELETE", "/api/v1/dashboards/d1", nil), http.StatusNoContent)
	wantError(t, admin.do("DELETE", "/api/v1/dashboards/d1", nil), http.StatusNotFound, errNotFound)
	// Two allowed decisions: the foreign attempt passed authorization before the owner check.
	if auditCount(f, authz.ActionDelete, authz.OutcomeAllowed) != 2 {
		t.Fatal("delete must be audited")
	}
}

func TestDashboardPermissionsAreEnforced(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	f.srv.cfg.Monitoring = fm
	fm.dash["shared"] = metrics.Dashboard{ID: "shared", Title: "S", Owner: "someone", Shared: true, Panels: []metrics.Panel{}}
	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.Global)
	wantIDs(t, ids(t, viewer.do("GET", "/api/v1/dashboards", nil), func(d metrics.Dashboard) string { return d.ID }), "shared")
	wantError(t, viewer.do("POST", "/api/v1/dashboards/v1", dashBody("x", false)), http.StatusForbidden, errForbidden)
	if auditCount(f, authz.ActionCreate, authz.OutcomeDenied) != 1 {
		t.Fatal("denied create must be audited")
	}
	noRole := f.loggedIn("nobody", "", authz.Global)
	wantIDs(t, ids(t, noRole.do("GET", "/api/v1/dashboards", nil), func(d metrics.Dashboard) string { return d.ID }))
	// state-changing endpoints fail closed when auditing is down
	admin, _, _ := f.setupAdmin("admin")
	f.sink.SetErr(errors.New("audit down"))
	wantError(t, admin.do("POST", "/api/v1/dashboards/a1", dashBody("x", false)), http.StatusServiceUnavailable, errUnavailable)
	if _, ok := fm.dash["a1"]; ok {
		t.Fatal("dashboard created although the audit record failed")
	}
}

func TestMetricsQueryIsFilteredByViewerPermissions(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	fm.series = []metrics.Labels{
		metrics.MustLabels("dcm_galera_cluster_size", "swarm", "s1", "cluster", "c1", "node", "n1"),
		metrics.MustLabels("dcm_galera_cluster_size", "swarm", "s2", "cluster", "c2", "node", "n1"),
		metrics.MustLabels("dcm_galera_cluster_size", "collector", "galera"),
	}
	f.srv.cfg.Monitoring = fm
	q := map[string]any{"selector": "dcm_galera_cluster_size", "start": 1000, "end": 31000, "stepSeconds": 15}

	admin, _, _ := f.setupAdmin("admin")
	var res QueryResponse
	r := admin.do("POST", "/api/v1/metrics/query", q)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if len(res.Series) != 3 {
		t.Fatalf("admin sees %d series", len(res.Series))
	}

	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	r = viewer.do("POST", "/api/v1/metrics/query", q)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if len(res.Series) != 1 || res.Series[0].Labels["cluster"] != "c1" || res.Series[0].Labels["name"] != "dcm_galera_cluster_size" {
		t.Fatalf("%+v", res.Series)
	}
	// gaps are explicit and unavailable points carry no value
	if len(res.Gaps) != 1 || res.Gaps[0].Reason != "expired" {
		t.Fatalf("gaps %+v", res.Gaps)
	}
	pts := res.Series[0].Points
	if pts[0].State != "ok" || pts[0].V == nil || *pts[0].V != 3 || pts[1].State != "unavailable" || pts[1].V != nil {
		t.Fatalf("points %+v", pts)
	}
	if res.StepSeconds != 15 {
		t.Fatalf("step %d", res.StepSeconds)
	}
	noRole := f.loggedIn("nobody", "", authz.Global)
	r = noRole.do("POST", "/api/v1/metrics/query", q)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if len(res.Series) != 0 {
		t.Fatal("account without a role must see no series")
	}
}

func TestMetricsQueryValidationAndErrors(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	f.srv.cfg.Monitoring = fm
	admin, _, _ := f.setupAdmin("admin")
	ok := func() map[string]any {
		return map[string]any{"selector": "m", "start": 0, "end": 1000, "stepSeconds": 1}
	}
	for name, mut := range map[string]func(map[string]any){
		"bad selector": func(m map[string]any) { m["selector"] = "m{" },
		"zero step":    func(m map[string]any) { m["stepSeconds"] = 0 },
		"end<start":    func(m map[string]any) { m["end"] = -5 },
		"bad func":     func(m map[string]any) { m["func"] = "median" },
		"unknown key":  func(m map[string]any) { m["authorizer"] = "allow" },
	} {
		m := ok()
		mut(m)
		if r := admin.do("POST", "/api/v1/metrics/query", m); r.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, r.Code)
		}
	}
	for err, want := range map[error]int{
		metrics.ErrTooManySamples: http.StatusBadRequest, metrics.ErrInvalidQuery: http.StatusBadRequest,
		metrics.ErrBusy: http.StatusTooManyRequests, errors.New("disk"): http.StatusServiceUnavailable, ErrUnavailable: http.StatusServiceUnavailable,
	} {
		fm.qerr = err
		if r := admin.do("POST", "/api/v1/metrics/query", ok()); r.Code != want {
			t.Errorf("%v: status %d, want %d", err, r.Code, want)
		}
	}
}

func TestNotificationsFilteredAndAckAudited(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	fm.notes = []Notification{
		{ID: "n1", Time: now, Severity: "critical", Kind: "alert.firing", Swarm: "s1", Cluster: "c1", Message: "a"},
		{ID: "n2", Time: now, Severity: "warning", Kind: "alert.firing", Swarm: "s2", Cluster: "c2", Message: "b", Read: true},
		{ID: "n3", Time: now, Severity: "unknown", Kind: "alert.unknown", Message: "manager"},
	}
	f.srv.cfg.Monitoring = fm
	admin, _, _ := f.setupAdmin("admin")
	nid := func(n Notification) string { return n.ID }
	wantIDs(t, ids(t, admin.do("GET", "/api/v1/notifications", nil), nid), "n1", "n2", "n3")
	wantIDs(t, ids(t, admin.do("GET", "/api/v1/notifications?unread=true", nil), nid), "n1", "n3")

	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	wantIDs(t, ids(t, viewer.do("GET", "/api/v1/notifications", nil), nid), "n1")
	wantError(t, viewer.do("POST", "/api/v1/notifications/n1/ack", nil), http.StatusNotFound, errNotFound)

	op := f.loggedIn("op", authz.RoleOperator, authz.ClusterScope("s1", "c1"))
	wantStatus(t, op.do("POST", "/api/v1/notifications/n1/ack", nil), http.StatusNoContent)
	if len(fm.acked) != 1 || fm.acked[0] != "n1/op" || auditCount(f, authz.ActionOperate, authz.OutcomeAllowed) != 1 {
		t.Fatalf("acked %v", fm.acked)
	}
	// no existence leak for a notification of another cluster
	wantError(t, op.do("POST", "/api/v1/notifications/n2/ack", nil), http.StatusNotFound, errNotFound)
	wantError(t, op.do("POST", "/api/v1/notifications/zzz/ack", nil), http.StatusNotFound, errNotFound)
}

func TestMonitoringRejectsCrossOriginWrites(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.Monitoring = newFakeMonitoring()
	admin, _, _ := f.setupAdmin("admin")
	admin.origin = "https://evil.example"
	wantError(t, admin.do("POST", "/api/v1/metrics/query", map[string]any{}), http.StatusForbidden, errOriginRejected)
	wantError(t, admin.do("POST", "/api/v1/dashboards/x", dashBody("x", false)), http.StatusForbidden, errOriginRejected)
}
