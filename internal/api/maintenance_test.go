// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type fakeMaintenance struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeMaintenance) rec(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeMaintenance) MaintenancePlan(_ context.Context, _ authz.Subject, swarm, node string, end bool) (MaintenancePreview, error) {
	return MaintenancePreview{Host: node, End: end, Safe: true}, f.rec("plan:" + swarm + "/" + node)
}

func (f *fakeMaintenance) EnterMaintenance(_ context.Context, actor authz.Subject, swarm, node, reason string) (MaintenanceStarted, error) {
	return MaintenanceStarted{Host: node, Operations: []OperationRef{{ID: "op-m", Kind: "galera/host-maintenance"}}},
		f.rec("enter:" + actor.ID + ":" + swarm + "/" + node + ":" + reason)
}

func (f *fakeMaintenance) LeaveMaintenance(_ context.Context, actor authz.Subject, swarm, node string) (MaintenanceStarted, error) {
	return MaintenanceStarted{Host: node}, f.rec("leave:" + actor.ID + ":" + swarm + "/" + node)
}

func (f *fakeMaintenance) ClusterDrift(_ context.Context, _ authz.Subject, id string) (DriftReport, error) {
	return DriftReport{Cluster: id, InSync: true}, f.rec("drift:" + id)
}

type logicalBackups struct {
	*fakeBackups
	dbs []string
}

func (l *logicalBackups) StartLogicalBackup(_ context.Context, a authz.Subject, c string, dbs []string) (OperationRef, error) {
	l.dbs = dbs
	return OperationRef{ID: "op-l", Kind: "dbbackup/backup"}, nil
}

// TestBackupStartMethods: the optional body selects a logical backup of
// named databases; malformed combinations are refused before the service,
// and a service without logical support answers 503.
func TestBackupStartMethods(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("POST", "/api/v1/clusters/c1/backups", map[string]any{"method": "logical", "databases": []string{"shop"}}),
		http.StatusServiceUnavailable, errUnavailable)
	lb := &logicalBackups{fakeBackups: cf.bak}
	cf.srv.cfg.Backups = lb
	var ref OperationRef
	r := admin.do("POST", "/api/v1/clusters/c1/backups", map[string]any{"method": "logical", "databases": []string{"shop", "crm"}})
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &ref)
	if ref.ID != "op-l" || len(lb.dbs) != 2 {
		t.Fatalf("%+v %v", ref, lb.dbs)
	}
	for _, body := range []map[string]any{{"method": "logical"}, {"method": "physical", "databases": []string{"x"}}, {"method": "snapshot"}} {
		wantError(t, admin.do("POST", "/api/v1/clusters/c1/backups", body), http.StatusBadRequest, errBadRequest)
	}
	wantStatus(t, admin.do("POST", "/api/v1/clusters/c1/backups", nil), http.StatusAccepted)
}

// TestMaintenanceAndDriftRoutes: maintenance needs host operate (viewers
// are refused, operators allowed), the plan and drift need read (hidden
// otherwise), state-changing calls are audited, and conflicts map to 409.
func TestMaintenanceAndDriftRoutes(t *testing.T) {
	cf := newControlFixture(t)
	fm := &fakeMaintenance{}
	cf.srv.cfg.Maintenance = fm
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	wantError(t, viewer.do("POST", "/api/v1/hosts/h1/maintenance", map[string]string{"reason": "x"}), http.StatusForbidden, errForbidden)
	wantStatus(t, viewer.do("GET", "/api/v1/hosts/h1/maintenance-plan", nil), http.StatusOK)
	wantStatus(t, viewer.do("GET", "/api/v1/clusters/c1/drift", nil), http.StatusOK)
	other := cf.loggedIn("v2", authz.RoleViewer, authz.SwarmScope("s2"))
	wantError(t, other.do("GET", "/api/v1/hosts/h1/maintenance-plan", nil), http.StatusNotFound, errNotFound)
	wantError(t, other.do("GET", "/api/v1/clusters/c1/drift", nil), http.StatusNotFound, errNotFound)

	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	var st MaintenanceStarted
	r := op.do("POST", "/api/v1/hosts/h1/maintenance", map[string]string{"reason": "kernel"})
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &st)
	if len(st.Operations) != 1 || st.Preview.Clusters == nil {
		t.Fatalf("%+v", st)
	}
	wantError(t, op.do("POST", "/api/v1/hosts/h1/maintenance", map[string]string{"reason": strings.Repeat("x", 501)}), http.StatusBadRequest, errBadRequest)
	wantStatus(t, op.do("DELETE", "/api/v1/hosts/h1/maintenance", nil), http.StatusAccepted)
	if n := auditsOf(cf.fixture, authz.ActionOperate, authz.OutcomeAllowed, authz.ResourceHost); n < 2 {
		t.Fatalf("allowed maintenance audited %d times", n)
	}
	if n := auditsOf(cf.fixture, authz.ActionOperate, authz.OutcomeDenied, authz.ResourceHost); n != 1 {
		t.Fatalf("denied maintenance audited %d times", n)
	}
	fm.err = ErrConflict
	wantError(t, op.do("POST", "/api/v1/hosts/h1/maintenance", nil), http.StatusConflict, errConflict)
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if !strings.Contains(strings.Join(fm.calls, " "), "enter:op:s1/h1:kernel") {
		t.Fatalf("calls %v", fm.calls)
	}
}
