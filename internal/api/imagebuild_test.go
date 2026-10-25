// SPDX-License-Identifier: AGPL-3.0-only

package api

// Fakes only: FakeBuilder, in-memory stores and an httptest registry.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

type imageFixture struct {
	*controlFixture
	builder     *imagebuild.FakeBuilder
	regs        *registry.Service
	backend     *ImageBuildBackend
	secrets     *registry.MemorySecretStore
	registryURL string
}

func newImageFixture(t *testing.T) *imageFixture {
	cf := newControlFixture(t)
	// A minimal registry that accepts the ping and basic credentials.
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "pw-api-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(reg.Close)
	secrets := &registry.MemorySecretStore{}
	regs := &registry.Service{Configs: &registry.MemoryConfigStore{}, Secrets: secrets}
	fb := &imagebuild.FakeBuilder{}
	builds := &imagebuild.Service{Builder: fb, Jobs: &imagebuild.MemoryJobStore{}, Registries: regs}
	be := &ImageBuildBackend{Builds: builds, Regs: regs}
	cf.srv.cfg.ImageBuilds, cf.srv.cfg.Registries = be, be
	f := &imageFixture{controlFixture: cf, builder: fb, regs: regs, backend: be, secrets: secrets}
	f.registryURL = reg.URL
	return f
}

func (f *imageFixture) body(extra map[string]any) map[string]any {
	m := map[string]any{
		"spec": map[string]any{
			"name":  "shop",
			"nginx": map[string]any{"version": "1.27", "baseDigest": "sha256:" + strings.Repeat("a", 64)},
			"php":   map[string]any{"version": "8.3", "extensions": []string{"mysqli"}},
		},
		"target": map[string]any{"registry": "main", "repository": "team/shop", "tag": "1"},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestImageBuildRoutesEndToEnd(t *testing.T) {
	f := newImageFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)

	// Registering an external registry: administrator only, credentials checked.
	reg := map[string]any{"id": "main", "name": "Main", "kind": "generic", "endpoint": f.registryURL, "username": "bot", "password": "pw-api-secret"}
	wantError(t, op.do("POST", "/api/v1/registries", reg), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("POST", "/api/v1/registries", reg), http.StatusForbidden, errForbidden)
	bad := map[string]any{"id": "main", "name": "Main", "kind": "generic", "endpoint": f.registryURL, "username": "bot", "password": "wrong"}
	wantError(t, admin.do("POST", "/api/v1/registries", bad), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/registries", map[string]any{"id": "x"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/registries", map[string]any{"id": "m", "name": "m", "kind": "managed"}), http.StatusBadRequest, errBadRequest)
	var rv RegistryView
	r := admin.do("POST", "/api/v1/registries", reg)
	wantStatus(t, r, http.StatusCreated)
	r.decode(t, &rv)
	if rv.ID != "main" || !rv.CredentialSet || rv.CreatedBy == "" {
		t.Fatalf("%+v", rv)
	}
	if strings.Contains(r.Body.String(), "pw-api-secret") {
		t.Fatal("password in response")
	}
	wantError(t, admin.do("POST", "/api/v1/registries", reg), http.StatusConflict, errConflict)
	cfg, err := f.regs.Configs.Get(context.Background(), "main")
	if err != nil || cfg.CredentialRef == "" {
		t.Fatalf("registry config: %+v, %v", cfg, err)
	}
	if sec, _ := f.secrets.Get(context.Background(), cfg.CredentialRef); sec.Reveal() != "pw-api-secret" {
		t.Fatal("credential not stored in the secret store")
	}
	var list []RegistryView
	r = viewer.do("GET", "/api/v1/registries", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list) != 1 || strings.Contains(r.Body.String(), "pw-api-secret") {
		t.Fatalf("%v", list)
	}

	// Plan preview: allowed for operators, builds nothing.
	var res ImageBuildResult
	r = op.do("POST", "/api/v1/image-builds", f.body(nil))
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if res.Plan == nil || res.Job != nil || res.Plan.Digest == "" || !strings.Contains(res.Plan.Dockerfile, "FROM docker.io/library/nginx:1.27-alpine@") ||
		res.Plan.Reference != strings.TrimPrefix(f.registryURL, "http://")+"/team/shop:1" {
		t.Fatalf("%+v", res.Plan)
	}
	if len(f.builder.Calls) != 0 {
		t.Fatal("preview built something")
	}
	// Viewers cannot plan or build; the denial is audited.
	wantError(t, viewer.do("POST", "/api/v1/image-builds", f.body(nil)), http.StatusForbidden, errForbidden)
	if auditsOf(f.fixture, authz.ActionCreate, authz.OutcomeDenied, authz.ResourceImageBuild) != 1 {
		t.Fatal("denied image build not audited")
	}
	// Invalid compositions and unknown registries.
	badSpec := f.body(nil)
	badSpec["spec"].(map[string]any)["nginx"] = map[string]any{"version": "1.27"}
	wantError(t, op.do("POST", "/api/v1/image-builds", badSpec), http.StatusBadRequest, errBadRequest)
	unknown := f.body(map[string]any{"target": map[string]any{"registry": "nope", "repository": "a", "tag": "1"}})
	wantError(t, op.do("POST", "/api/v1/image-builds", unknown), http.StatusNotFound, errNotFound)
	wantError(t, op.do("POST", "/api/v1/image-builds", map[string]any{"spec": 1}), http.StatusBadRequest, errBadRequest)

	// Approval: wrong digest refused, right digest builds.
	wantError(t, op.do("POST", "/api/v1/image-builds", f.body(map[string]any{"approvalDigest": "sha256:" + strings.Repeat("0", 64)})),
		http.StatusConflict, errApprovalMismatch)
	if len(f.builder.Calls) != 0 {
		t.Fatal("unapproved build ran")
	}
	r = op.do("POST", "/api/v1/image-builds", f.body(map[string]any{"approvalDigest": res.Plan.Digest}))
	wantStatus(t, r, http.StatusAccepted)
	var started ImageBuildResult
	r.decode(t, &started)
	if started.Job == nil || started.Job.Status != imagebuild.JobSucceeded || started.Job.SpecHash != res.Plan.SpecHash || started.Job.ImageDigest == "" {
		t.Fatalf("%+v", started.Job)
	}
	if strings.Contains(r.Body.String(), "pw-api-secret") {
		t.Fatal("password in job response")
	}
	if auditsOf(f.fixture, authz.ActionCreate, authz.OutcomeAllowed, authz.ResourceImageBuild) < 2 {
		t.Fatal("allowed image build decisions not audited")
	}
	var jobs []ImageBuildJob
	r = viewer.do("GET", "/api/v1/image-builds", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &jobs)
	if len(jobs) != 1 || jobs[0].Log != "" || jobs[0].RequestedBy == "" {
		t.Fatalf("%+v", jobs)
	}
}

func TestImageBuildRoutesUnavailableWithoutServices(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/image-builds", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("GET", "/api/v1/registries", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("POST", "/api/v1/image-builds", map[string]any{"spec": map[string]any{}, "target": map[string]any{}}),
		http.StatusServiceUnavailable, errUnavailable)
}
