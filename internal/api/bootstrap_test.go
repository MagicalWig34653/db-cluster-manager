// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// fakeBootstrap records calls; it never contacts a host.
type fakeBootstrap struct {
	calls []string
	last  provision.DraftInput
	err   error
}

func (f *fakeBootstrap) view(id string, st provision.Status) provision.BootstrapView {
	return provision.BootstrapView{ID: id, Status: st, Progress: []provision.StepProgress{}}
}
func (f *fakeBootstrap) Draft(_ context.Context, in provision.DraftInput) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "draft")
	f.last = in
	if f.err != nil {
		return provision.BootstrapView{}, f.err
	}
	v := f.view(in.Request.ID, provision.StatusAwaitingFingerprint)
	v.HostKey = &provision.HostKey{Type: "ssh-ed25519", Fingerprint: "SHA256:abc"}
	return v, nil
}
func (f *fakeBootstrap) ConfirmFingerprint(_ context.Context, id, fp string) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "fingerprint")
	if fp != "SHA256:abc" {
		return provision.BootstrapView{}, provision.ErrHostKeyMismatch
	}
	return f.view(id, provision.StatusPlanned), nil
}
func (f *fakeBootstrap) Preview(id string) (provision.BootstrapPlan, error) {
	f.calls = append(f.calls, "preview")
	return provision.BootstrapPlan{ID: id, Steps: []provision.BootstrapStep{}, Findings: []provision.BootstrapFinding{}}, nil
}
func (f *fakeBootstrap) Approve(id, d string) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "approve")
	if d != strings.Repeat("a", 64) {
		return provision.BootstrapView{}, provision.ErrDigest
	}
	return f.view(id, provision.StatusApproved), nil
}
func (f *fakeBootstrap) ApproveLockdown(id, d string) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "approve-lockdown")
	return f.view(id, provision.StatusMainComplete), nil
}
func (f *fakeBootstrap) SupplyCredentials(id string, c provision.Credential) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "credentials")
	return f.view(id, provision.StatusPlanned), nil
}
func (f *fakeBootstrap) Run(_ context.Context, id string) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "run")
	return f.view(id, provision.StatusRunning), nil
}
func (f *fakeBootstrap) Status(id string) provision.BootstrapView {
	return f.view(id, provision.StatusPlanned)
}
func (f *fakeBootstrap) List() []provision.BootstrapView { return []provision.BootstrapView{} }
func (f *fakeBootstrap) Cancel(id string) (provision.BootstrapView, error) {
	f.calls = append(f.calls, "cancel")
	return f.view(id, provision.StatusCancelled), nil
}

func bootBody() map[string]any {
	return map[string]any{
		"id":         "boot1",
		"host":       map[string]any{"id": "fresh1", "address": "fresh1.example.test", "user": "root", "role": "worker"},
		"profile":    map[string]any{"serviceUser": "dbcm", "runtime": "docker", "cluster": map[string]any{"platform": "none"}},
		"credential": map[string]any{"password": "synthetic-root-password"},
	}
}

func TestBootstrapRoutesAuthzAndSecrets(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	paths := []struct{ method, path string }{
		{"GET", "/api/v1/bootstrap/support"},
		{"GET", "/api/v1/bootstrap/drafts"},
		{"POST", "/api/v1/bootstrap/drafts"},
		{"GET", "/api/v1/bootstrap/drafts/boot1"},
		{"GET", "/api/v1/bootstrap/drafts/boot1/plan"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/fingerprint"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/approve"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/approve-lockdown"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/credentials"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/run"},
		{"POST", "/api/v1/bootstrap/drafts/boot1/cancel"},
	}
	// No backend configured: 503 for the administrator, 401 anonymous.
	for _, p := range paths {
		var body any
		if p.method == "POST" {
			body = bootBody()
		}
		wantError(t, admin.do(p.method, p.path, body), http.StatusServiceUnavailable, errUnavailable)
		wantError(t, f.client().do(p.method, p.path, body), http.StatusUnauthorized, errUnauthenticated)
	}
	be := &fakeBootstrap{}
	f.srv.cfg.Bootstrap = be
	for _, p := range paths {
		var body any
		if p.method == "POST" {
			body = bootBody()
		}
		wantError(t, op.do(p.method, p.path, body), http.StatusForbidden, errForbidden)
		wantError(t, viewer.do(p.method, p.path, body), http.StatusForbidden, errForbidden)
	}
	if len(be.calls) != 0 {
		t.Fatalf("a denied caller reached the backend: %v", be.calls)
	}

	r := admin.do("GET", "/api/v1/bootstrap/support", nil)
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.Body.String(), "almalinux") || !strings.Contains(r.Body.String(), "alpine") {
		t.Fatal(r.Body.String())
	}

	r = admin.do("POST", "/api/v1/bootstrap/drafts", bootBody())
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "synthetic-root-password") || !strings.Contains(r.Body.String(), "SHA256:abc") {
		t.Fatal("draft response", r.Body.String())
	}
	if be.last.Credential.Password != "synthetic-root-password" {
		t.Fatal("credential not passed to the backend")
	}
	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts", map[string]any{"id": "x"}), http.StatusBadRequest, errBadRequest)

	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/fingerprint", map[string]string{"fingerprint": "SHA256:other"}), http.StatusConflict, errConflict)
	wantStatus(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/fingerprint", map[string]string{"fingerprint": "SHA256:abc"}), http.StatusOK)
	wantStatus(t, admin.do("GET", "/api/v1/bootstrap/drafts/boot1/plan", nil), http.StatusOK)
	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/approve", map[string]string{"digest": strings.Repeat("b", 64)}), http.StatusConflict, errApprovalMismatch)
	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/approve", map[string]string{"digest": "short"}), http.StatusBadRequest, errBadRequest)
	wantStatus(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/approve", map[string]string{"digest": strings.Repeat("a", 64)}), http.StatusOK)
	wantStatus(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/run", nil), http.StatusAccepted)
	wantStatus(t, admin.do("POST", "/api/v1/bootstrap/drafts/boot1/cancel", nil), http.StatusOK)

	be.err = provision.ErrExistingCluster
	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts", bootBody()), http.StatusConflict, errConflict)
	be.err = provision.ErrInvalid
	wantError(t, admin.do("POST", "/api/v1/bootstrap/drafts", bootBody()), http.StatusBadRequest, errBadRequest)
}
