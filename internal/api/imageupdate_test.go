// SPDX-License-Identifier: AGPL-3.0-only

package api

// Fakes only: a fake service source, resolver and driver behind a real
// imageupdate.Service on the in-memory KV.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

const (
	iuA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	iuB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

type iuSource struct{}

func (iuSource) Services(context.Context) ([]imageupdate.ManagedService, error) {
	return []imageupdate.ManagedService{
		{ID: "web", Name: "web", Swarm: "s1", Type: "container", Image: "nginx:1.25.3@" + iuA, Labels: map[string]string{imageupdate.LabelInstallation: "inst-1"}},
		{ID: "gal", Name: "gal", Swarm: "s1", Type: "galera", Image: "mariadb:11.4.2@" + iuA, Labels: map[string]string{imageupdate.LabelInstallation: "inst-1"}},
		{ID: "alien", Name: "alien", Type: "container", Image: "nginx:1.25.3", Labels: map[string]string{imageupdate.LabelInstallation: "other"}},
	}, nil
}

type iuResolver struct{}

func (iuResolver) Tags(_ context.Context, r imageupdate.Ref) ([]string, error) {
	if strings.Contains(r.Repo, "mariadb") {
		return []string{"11.4.2", "11.4.3"}, nil
	}
	return []string{"1.25.3", "1.25.4"}, nil
}
func (iuResolver) Digest(_ context.Context, r imageupdate.Ref) (string, error) {
	if r.Tag == "1.25.4" || r.Tag == "11.4.3" {
		return iuB, nil
	}
	return iuA, nil
}

type iuDriver struct {
	updates  int
	startErr error
}

func (d *iuDriver) StartUpdate(context.Context, string, string, string, imageupdate.Strategy) (imageupdate.Started, error) {
	if d.startErr != nil {
		return imageupdate.Started{}, d.startErr
	}
	d.updates++
	return imageupdate.Started{OperationID: "op-1", Digest: "dg"}, nil
}
func (d *iuDriver) StartRollback(context.Context, string, string, imageupdate.Strategy) (imageupdate.Started, error) {
	return imageupdate.Started{OperationID: "op-rb", Digest: "dg"}, nil
}
func (d *iuDriver) Approve(context.Context, string, string, string) error { return nil }
func (d *iuDriver) Status(context.Context, string) (imageupdate.OpStatus, error) {
	return imageupdate.OpStatus{State: imageupdate.OpSucceeded, Runtime: "container"}, nil
}

type iuBuilds struct{}

func (b *iuBuilds) Built(_ context.Context, jobID, findingID string) (imageupdate.Built, error) {
	if jobID == "ib-missing" {
		return imageupdate.Built{}, ErrNotFound
	}
	return imageupdate.Built{JobID: jobID, Reference: "nginx:1.25.4", Digest: iuB, FindingID: findingID}, nil
}

func newIUFixture(t *testing.T) (*fixture, *imageupdate.Service, *iuDriver) {
	f := newFixture(t)
	drv := &iuDriver{}
	svc := &imageupdate.Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Source: iuSource{}, Driver: drv, Resolver: iuResolver{}, Installation: "inst-1",
		Term: func() (uint64, error) { return 3, nil }}
	f.srv.cfg.ImageUpdates = &ImageUpdateBackend{Svc: svc, Builds: &iuBuilds{}}
	return f, svc, drv
}

func TestImageUpdateUnavailableWithoutBackend(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/image-updates/services", nil), http.StatusServiceUnavailable, errUnavailable)
}

func TestImageUpdateRoleMatrix(t *testing.T) {
	f, _, _ := newIUFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	dba := f.loggedIn("dba", authz.RoleDatabaseAdmin, authz.Global)

	type route struct{ m, p string }
	reads := []route{{"GET", "/image-updates/services"}, {"GET", "/image-updates/plans"}, {"GET", "/image-updates/plans/x"}}
	operates := []route{{"POST", "/image-updates/services/web/check"}}
	admins := []route{{"PUT", "/image-updates/services/web/policy"}, {"DELETE", "/image-updates/services/web/policy"},
		{"POST", "/image-updates/plans/x/approve"}, {"POST", "/image-updates/plans/x/decline"}, {"POST", "/image-updates/plans/x/apply"},
		{"POST", "/image-updates/plans/x/rollback"}, {"POST", "/image-updates/offers"}}
	forbidden := func(c *client, rs []route) {
		t.Helper()
		for _, r := range rs {
			wantError(t, c.do(r.m, "/api/v1"+r.p, map[string]any{}), http.StatusForbidden, errForbidden)
		}
	}
	allowed := func(c *client, rs []route) {
		t.Helper()
		for _, r := range rs {
			resp := c.do(r.m, "/api/v1"+r.p, map[string]any{})
			if resp.Code == http.StatusForbidden || resp.Code == http.StatusUnauthorized {
				t.Fatalf("%s %s denied for a role that should pass: %d", r.m, r.p, resp.Code)
			}
		}
	}
	forbidden(dba, append(append(append([]route{}, reads...), operates...), admins...))
	allowed(viewer, reads[:2])
	forbidden(viewer, append(append([]route{}, operates...), admins...))
	allowed(op, append(append([]route{}, reads[:2]...), operates...))
	forbidden(op, admins)
	allowed(admin, reads[:2])
}

func TestImageUpdateEndToEnd(t *testing.T) {
	f, _, drv := newIUFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)

	var views []imageupdate.ServiceView
	op.do("GET", "/api/v1/image-updates/services", nil).decode(t, &views)
	if len(views) != 2 {
		t.Fatalf("foreign-installation service listed: %+v", views)
	}
	wantError(t, admin.do("PUT", "/api/v1/image-updates/services/alien/policy", map[string]any{"mode": "notify"}), http.StatusNotFound, errNotFound)
	wantError(t, admin.do("PUT", "/api/v1/image-updates/services/web/policy", map[string]any{"mode": "yolo"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("PUT", "/api/v1/image-updates/services/web/policy", map[string]any{"mode": "notify", "extra": 1}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("PUT", "/api/v1/image-updates/services/gal/policy", map[string]any{"mode": "auto", "track": "patch",
		"windows": []map[string]any{{"start": "02:00", "durationMinutes": 60}}}), http.StatusBadRequest, errBadRequest)
	wantStatus(t, admin.do("PUT", "/api/v1/image-updates/services/web/policy", map[string]any{"mode": "notify", "track": "patch"}), http.StatusOK)

	var res imageupdate.CheckResult
	r := op.do("POST", "/api/v1/image-updates/services/web/check", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if res.Plan == nil || res.Plan.To.Tag != "1.25.4" || res.Plan.State != imageupdate.PlanProposed {
		t.Fatalf("%+v", res)
	}
	wantError(t, op.do("POST", "/api/v1/image-updates/services/alien/check", nil), http.StatusNotFound, errNotFound)

	pid := res.Plan.ID
	wantError(t, op.do("POST", "/api/v1/image-updates/plans/"+pid+"/approve", map[string]any{"digest": res.Plan.Digest}), http.StatusForbidden, errForbidden)
	wantError(t, admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/approve", map[string]any{"digest": "short"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/approve", map[string]any{"digest": strings.Repeat("0", 64)}), http.StatusConflict, errApprovalMismatch)
	wantError(t, admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/apply", nil), http.StatusConflict, errConflict)
	wantStatus(t, admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/approve", map[string]any{"digest": res.Plan.Digest}), http.StatusOK)
	if drv.updates != 0 {
		t.Fatal("approval applied the update")
	}
	var plan imageupdate.Plan
	r = admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/apply", nil)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &plan)
	if plan.State != imageupdate.PlanSucceeded || drv.updates != 1 || plan.ExecutedBy != "container" {
		t.Fatalf("%+v", plan)
	}
	var plans []imageupdate.Plan
	op.do("GET", "/api/v1/image-updates/plans", nil).decode(t, &plans)
	if len(plans) != 1 {
		t.Fatalf("%+v", plans)
	}
	wantError(t, op.do("GET", "/api/v1/image-updates/plans/nope", nil), http.StatusNotFound, errNotFound)

	// Rollback needs a running image equal to the plan's target; the fake
	// source still reports the old image, so it is refused as stale.
	wantError(t, admin.do("POST", "/api/v1/image-updates/plans/"+pid+"/rollback", nil), http.StatusConflict, errConflict)

	// The Galera cluster is routed to its planner.
	wantStatus(t, admin.do("PUT", "/api/v1/image-updates/services/gal/policy", map[string]any{"mode": "notify", "track": "patch"}), http.StatusOK)
	r = op.do("POST", "/api/v1/image-updates/services/gal/check", nil)
	wantStatus(t, r, http.StatusOK)
	res = imageupdate.CheckResult{}
	r.decode(t, &res)
	if res.Plan == nil || res.Plan.Route != imageupdate.RoutePlanner {
		t.Fatalf("%+v", res)
	}
	wantError(t, admin.do("POST", "/api/v1/image-updates/plans/"+res.Plan.ID+"/approve", map[string]any{"digest": res.Plan.Digest}), http.StatusConflict, errPolicyConflict)
}

func TestImageUpdateOfferFromBuild(t *testing.T) {
	f, _, _ := newIUFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	var plans []imageupdate.Plan
	r := admin.do("POST", "/api/v1/image-updates/offers", map[string]any{"jobId": "ib-1", "findingId": "f-1"})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &plans)
	if len(plans) != 1 || plans[0].Kind != imageupdate.KindBuilt || plans[0].State != imageupdate.PlanProposed || plans[0].ServiceID != "web" {
		t.Fatalf("%+v", plans)
	}
	wantError(t, admin.do("POST", "/api/v1/image-updates/offers", map[string]any{"jobId": "ib-missing"}), http.StatusNotFound, errNotFound)
	wantError(t, admin.do("POST", "/api/v1/image-updates/offers", map[string]any{}), http.StatusBadRequest, errBadRequest)
}

func TestImageUpdateNoSecretsAndErrorMapping(t *testing.T) {
	for err, want := range map[error]error{
		imageupdate.ErrNotFound: ErrNotFound, imageupdate.ErrNotOwned: ErrNotFound, imageupdate.ErrInvalid: ErrInvalidInput,
		imageupdate.ErrDigest: ErrApprovalMismatch, imageupdate.ErrConflict: ErrConflict, imageupdate.ErrStale: ErrConflict,
		imageupdate.ErrOutsideWindow: ErrConflict, imageupdate.ErrBusy: ErrBusy, imageupdate.ErrRoutedToPlanner: ErrPolicyConflict, imageupdate.ErrForeign: ErrPolicyConflict,
	} {
		if got := mapImageUpdateError(err); !errors.Is(got, want) {
			t.Fatalf("%v -> %v, want %v", err, got, want)
		}
	}
}
