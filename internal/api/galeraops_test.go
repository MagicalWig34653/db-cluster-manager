// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

type fakeClusterOperations struct {
	*fakeControl
	calls int
	err   error
}

func (f *fakeClusterOperations) PlanClusterOperation(_ context.Context, _ authz.Subject, id string, req galera.OperationRequest) (ClusterOperationPlan, error) {
	f.calls++
	return ClusterOperationPlan{ID: "op-plan", Kind: "galera/" + string(req.Kind), Approval: OperationApproval{Digest: digestA}}, f.err
}

func TestClusterOperationPlanContract(t *testing.T) {
	cf := newControlFixture(t)
	ctl := &fakeClusterOperations{fakeControl: cf.ctl}
	cf.srv.cfg.Control = ctl
	viewer := cf.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	req := map[string]any{"kind": "rolling-restart"}
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/operation-plan", req), http.StatusForbidden, errForbidden)
	if ctl.calls != 0 {
		t.Fatal("denied request reached control")
	}
	admin := cf.loggedIn("admin", authz.RoleAdministrator, authz.SwarmScope("s1"))
	wantError(t, admin.do("POST", "/api/v1/clusters/c2/operation-plan", req), http.StatusForbidden, errForbidden)
	var out ClusterOperationPlan
	r := admin.do("POST", "/api/v1/clusters/c1/operation-plan", req)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &out)
	if out.ID != "op-plan" || out.Approval.Digest != digestA {
		t.Fatalf("plan %+v", out)
	}
	wantError(t, admin.do("POST", "/api/v1/clusters/c1/operation-plan", map[string]any{"kind": "rolling-restart", "bootstrap": true}), http.StatusBadRequest, errBadRequest)
	ctl.err = ErrMajorUpgradeApproval
	wantError(t, admin.do("POST", "/api/v1/clusters/c1/operation-plan", req), http.StatusConflict, "major_upgrade_requires_product_approval")
	ctl.err = ErrRequiresGuidedRecovery
	wantError(t, admin.do("POST", "/api/v1/clusters/c1/operation-plan", req), http.StatusConflict, "requires_guided_recovery")
	for _, kind := range []string{"scale-in", "scale-out", "rolling-restart", "minor-upgrade"} {
		act, res := ApprovalPermission("galera/"+kind, "s1", "c1")
		if act != authz.ActionOperate || res.Type != authz.ResourceCluster || res.Scope != authz.ClusterScope("s1", "c1") {
			t.Fatalf("approval permission: %s %+v", act, res)
		}
	}
}
