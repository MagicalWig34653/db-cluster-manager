// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// Fakes only: the service backend is a fake; no manager or Swarm is behind it.

type fakeServices struct {
	mu    sync.Mutex
	calls []string
	err   error
	items []ServiceInfo
}

func (f *fakeServices) rec(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeServices) Services(context.Context) ([]ServiceInfo, error) { return f.items, nil }

func (f *fakeServices) Service(_ context.Context, id string) (ServiceInfo, error) {
	for _, s := range f.items {
		if s.ID == id {
			return s, nil
		}
	}
	return ServiceInfo{}, ErrNotFound
}

func (f *fakeServices) PreviewService(_ context.Context, a authz.Subject, in ServiceInput) (ServicePlan, error) {
	return ServicePlan{Kind: "create", Type: in.Type, Name: in.Name, Blockers: nil}, f.rec("preview:" + a.ID + ":" + in.Swarm + "/" + in.Name)
}

func (f *fakeServices) CreateService(_ context.Context, a authz.Subject, in ServiceInput) (ServiceStarted, error) {
	return ServiceStarted{Service: "svc-1", Operation: OperationRef{ID: "op-s", Kind: OperationKindServiceCreate},
		Approval: OperationApproval{Digest: "d"}}, f.rec("create:" + a.ID + ":" + in.Swarm + "/" + in.Name)
}

func (f *fakeServices) PreviewServiceDelete(_ context.Context, _ authz.Subject, id string, rv bool) (ServicePlan, error) {
	return ServicePlan{Kind: "delete"}, f.rec("delete-plan:" + id)
}

func (f *fakeServices) DeleteService(_ context.Context, a authz.Subject, id string, rv bool) (ServiceStarted, error) {
	s := "delete:" + a.ID + ":" + id
	if rv {
		s += ":volumes"
	}
	return ServiceStarted{Service: id, Operation: OperationRef{ID: "op-d", Kind: OperationKindServiceDelete}}, f.rec(s)
}

func TestServiceRoutes(t *testing.T) {
	cf := newControlFixture(t)
	fs := &fakeServices{items: []ServiceInfo{
		{ID: "svc-1", Swarm: "s1", Name: "web", Type: "container", Spec: json.RawMessage(`{"image":"nginx:1"}`)},
		{ID: "svc-2", Swarm: "s2", Name: "other", Type: "container", Spec: json.RawMessage(`{}`)},
	}}
	body := map[string]any{"swarm": "s1", "name": "web", "type": "container", "spec": map[string]any{"image": "nginx:1.27"}}

	// Without a backend every route answers 503, never an empty list.
	admin := cf.loggedIn("adm", authz.RoleAdministrator, authz.Global)
	wantError(t, admin.do("GET", "/api/v1/services", nil), http.StatusServiceUnavailable, errUnavailable)
	cf.srv.cfg.Services = fs

	// Viewer: lists only the services of its Swarm, reads one, cannot
	// preview, create, or delete (deny by default).
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	var list List[ServiceInfo]
	r := viewer.do("GET", "/api/v1/services", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != "svc-1" {
		t.Fatalf("%+v", list)
	}
	wantStatus(t, viewer.do("GET", "/api/v1/services/svc-1", nil), http.StatusOK)
	wantStatus(t, viewer.do("GET", "/api/v1/services/svc-1/delete-plan", nil), http.StatusOK)
	wantError(t, viewer.do("GET", "/api/v1/services/svc-2", nil), http.StatusNotFound, errNotFound)
	wantError(t, viewer.do("POST", "/api/v1/services/plan", body), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("POST", "/api/v1/services", body), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("DELETE", "/api/v1/services/svc-1", nil), http.StatusForbidden, errForbidden)
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	wantError(t, op.do("POST", "/api/v1/services", body), http.StatusForbidden, errForbidden)

	// Administrator in the Swarm: preview, create, delete; scoped to s1.
	sadm := cf.loggedIn("sadm", authz.RoleAdministrator, authz.SwarmScope("s1"))
	var pl ServicePlan
	r = sadm.do("POST", "/api/v1/services/plan", body)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &pl)
	if !pl.RequiresApproval || !pl.Executable || pl.Steps == nil || pl.Blockers == nil || pl.Artifacts == nil || pl.Warnings == nil {
		t.Fatalf("plan not normalized: %+v", pl)
	}
	var st ServiceStarted
	r = sadm.do("POST", "/api/v1/services", body)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &st)
	if st.Operation.ID != "op-s" || st.Approval.Digest != "d" || !st.Plan.RequiresApproval {
		t.Fatalf("%+v", st)
	}
	other := map[string]any{"swarm": "s2", "name": "x", "type": "container", "spec": map[string]any{}}
	wantError(t, sadm.do("POST", "/api/v1/services", other), http.StatusForbidden, errForbidden)
	for _, bad := range []map[string]any{
		{"swarm": "s1", "name": "Bad Name", "type": "container", "spec": map[string]any{}},
		{"swarm": "s1", "name": "web", "type": "container"},
		{"swarm": "", "name": "web", "type": "container", "spec": map[string]any{}},
		{"swarm": "s1", "name": "web", "type": "container", "spec": map[string]any{}, "extra": 1},
	} {
		wantError(t, sadm.do("POST", "/api/v1/services", bad), http.StatusBadRequest, errBadRequest)
	}
	wantStatus(t, sadm.do("DELETE", "/api/v1/services/svc-1", map[string]any{"removeVolumes": true}), http.StatusAccepted)
	wantStatus(t, sadm.do("DELETE", "/api/v1/services/svc-1", nil), http.StatusAccepted)
	wantError(t, sadm.do("DELETE", "/api/v1/services/svc-2", nil), http.StatusNotFound, errNotFound)
	fs.err = ErrConflict
	wantError(t, sadm.do("POST", "/api/v1/services", body), http.StatusConflict, errConflict)

	fs.mu.Lock()
	calls := append([]string(nil), fs.calls...)
	fs.mu.Unlock()
	for _, want := range []string{"preview:sadm:s1/web", "create:sadm:s1/web", "delete:sadm:svc-1:volumes", "delete:sadm:svc-1"} {
		found := false
		for _, c := range calls {
			found = found || c == want
		}
		if !found {
			t.Errorf("call %q missing in %v", want, calls)
		}
	}
	for _, c := range calls {
		if c == "create:v:s1/web" || c == "create:op:s1/web" {
			t.Errorf("denied caller reached the service: %s", c)
		}
	}
	// Denials of state-changing actions and the allowed creates are audited.
	if n := auditsOf(cf.fixture, authz.ActionCreate, authz.OutcomeDenied, authz.ResourceService); n < 3 {
		t.Errorf("denied service creates audited %d times", n)
	}
	if n := auditsOf(cf.fixture, authz.ActionCreate, authz.OutcomeAllowed, authz.ResourceService); n < 2 {
		t.Errorf("allowed service creates audited %d times", n)
	}
	if n := auditsOf(cf.fixture, authz.ActionDelete, authz.OutcomeAllowed, authz.ResourceService); n < 2 {
		t.Errorf("allowed service deletes audited %d times", n)
	}
}

// TestServiceApprovalPermission pins which permission approving a service
// operation needs.
func TestServiceApprovalPermission(t *testing.T) {
	for kind, want := range map[string]authz.Action{
		OperationKindServiceCreate: authz.ActionCreate,
		OperationKindServiceDelete: authz.ActionDelete,
	} {
		act, res := ApprovalPermission(kind, "s1", "")
		if act != want || res.Type != authz.ResourceService || res.Scope != authz.SwarmScope("s1") {
			t.Errorf("%s: %s on %+v", kind, act, res)
		}
	}
	// The cluster rules are unchanged.
	if act, res := ApprovalPermission(OperationKindClusterDelete, "s1", "c1"); act != authz.ActionDelete || res.Type != authz.ResourceCluster {
		t.Errorf("cluster delete: %s on %+v", act, res)
	}
}
