// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type fakeEngineUpdates struct{ exports int }

func (*fakeEngineUpdates) EngineUpdatePlan(_ context.Context, _ authz.Subject, swarm, host, target string) (EngineUpdatePlan, error) {
	return EngineUpdatePlan{Swarm: swarm, Host: host, CurrentVersion: "29.8.0", TargetVersion: target,
		AvailableVersions: []string{"29.9.1"}, Maintenance: MaintenancePreview{Host: host, Safe: true, Clusters: []ClusterMaintenancePlan{}},
		Steps: []string{}, Warnings: []string{}, Blockers: []string{}, RollbackNotes: []string{}, ApprovalRequired: true,
		Exportable: true, Digest: "sha256:" + strings.Repeat("a", 64)}, nil
}
func (f *fakeEngineUpdates) ExportEngineRunbook(_ context.Context, _ authz.Subject, _, _ string, in EngineRunbookRequest) (EngineRunbook, error) {
	f.exports++
	if in.Digest != "sha256:"+strings.Repeat("a", 64) {
		return EngineRunbook{}, ErrApprovalMismatch
	}
	return EngineRunbook{Digest: in.Digest, Script: "#!/bin/sh\n"}, nil
}

func TestEngineUpdateRouteContractAndAuthorization(t *testing.T) {
	cf := newControlFixture(t)
	fake := &fakeEngineUpdates{}
	cf.srv.cfg.EngineUpdates = fake
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	wantError(t, viewer.do("GET", "/api/v1/hosts/h1/engine-update-plan", nil), http.StatusNotFound, errNotFound)
	admin := cf.loggedIn("a", authz.RoleAdministrator, authz.Global)
	var plan EngineUpdatePlan
	r := admin.do("GET", "/api/v1/hosts/h1/engine-update-plan?targetVersion=29.9.1", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &plan)
	if plan.Host != "h1" || plan.TargetVersion != "29.9.1" || plan.Digest == "" {
		t.Fatalf("plan %+v", plan)
	}
	wantError(t, viewer.do("POST", "/api/v1/hosts/h1/engine-update-runbook", EngineRunbookRequest{TargetVersion: "29.9.1", Digest: plan.Digest}), http.StatusNotFound, errNotFound)
	wantError(t, admin.do("POST", "/api/v1/hosts/h1/engine-update-runbook", EngineRunbookRequest{TargetVersion: "29.9.1", Digest: "bad"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/hosts/h1/engine-update-runbook", EngineRunbookRequest{TargetVersion: "29.9.1", Digest: "sha256:" + strings.Repeat("0", 64)}), http.StatusConflict, errApprovalMismatch)
	wantStatus(t, admin.do("POST", "/api/v1/hosts/h1/engine-update-runbook", EngineRunbookRequest{TargetVersion: "29.9.1", Digest: plan.Digest}), http.StatusOK)
	if fake.exports != 2 || auditsOf(cf.fixture, authz.ActionOperate, authz.OutcomeAllowed, authz.ResourceEngineUpdate) == 0 {
		t.Fatalf("exports %d / missing audit", fake.exports)
	}
}
