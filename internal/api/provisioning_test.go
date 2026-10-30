package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

type fakeProvisioning struct{ calls int }

func (f *fakeProvisioning) Plan(_ context.Context, r provision.Request, c map[string]provision.Credential) (provision.Plan, error) {
	f.calls++
	return provision.Plan{ID: r.ID, Platform: r.Platform, Hosts: r.Hosts, Digest: strings.Repeat("a", 64), Scripts: map[string]string{"a": "echo safe"}}, nil
}
func (f *fakeProvisioning) Start(_ authz.Subject, id, digest string) (OperationRef, error) {
	f.calls++
	return OperationRef{ID: "op-1", Kind: "provision/swarm"}, nil
}
func TestProvisioningContractAndAuthz(t *testing.T) {
	fixture := newFixture(t)
	admin, _, _ := fixture.setupAdmin("admin")
	viewer := fixture.loggedIn("viewer", authz.RoleViewer, authz.Global)
	backend := &fakeProvisioning{}
	fixture.srv.cfg.Provisioning = backend
	body := map[string]any{"id": "lab", "platform": "swarm", "hosts": []map[string]any{{"id": "a", "address": "a.example.test", "user": "admin", "role": "manager"}}, "credentials": map[string]any{"a": map[string]string{"privateKey": "synthetic-private-key", "knownHosts": "a.example.test ssh-ed25519 synthetic"}}}
	reg := fixture.useModules(t, nil)
	wantError(t, admin.do("POST", "/api/v1/provision/plans", body), http.StatusNotFound, errModuleDisabled)
	if backend.calls != 0 {
		t.Fatal("disabled backend called")
	}
	if err := reg.Set("provisioning", true, "admin"); err != nil {
		t.Fatal(err)
	}
	wantError(t, viewer.do("POST", "/api/v1/provision/plans", body), http.StatusForbidden, errForbidden)
	r := admin.do("POST", "/api/v1/provision/plans", body)
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "synthetic-private-key") || strings.Contains(r.Body.String(), "knownHosts") {
		t.Fatal("secret leaked")
	}
	wantError(t, viewer.do("POST", "/api/v1/provision/plans/lab/execute", map[string]string{"digest": strings.Repeat("a", 64)}), http.StatusForbidden, errForbidden)
	r = admin.do("POST", "/api/v1/provision/plans/lab/execute", map[string]string{"digest": strings.Repeat("a", 64)})
	wantStatus(t, r, http.StatusAccepted)
	if backend.calls != 2 {
		t.Fatal("backend calls", backend.calls)
	}
	act, res := ApprovalPermission("provision/swarm", "", "")
	if act != authz.ActionOperate || res.Type != authz.ResourceSettings {
		t.Fatal(act, res)
	}
}
