// SPDX-License-Identifier: AGPL-3.0-only

package api

// Fakes only: FakeBuilder, in-memory stores and an httptest registry.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagecompose"
)

func composerRecipe() map[string]any {
	return map[string]any{
		"name": "shop",
		"base": map[string]any{"distro": "alpine", "release": "3.20", "digest": "sha256:" + strings.Repeat("a", 64)},
		"components": []any{
			map[string]any{"id": "nginx", "version": "1.26"},
			map[string]any{"id": "php", "version": "8.3", "features": []string{"mysqli", "opcache"},
				"settings": map[string]any{"memory_limit": "256M"}},
		},
	}
}

func TestImageComposerRoutes(t *testing.T) {
	f := newImageFixture(t)
	f.srv.cfg.ImageComposer = &ImageComposerBackend{Builds: f.backend.Builds}
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)

	reg := map[string]any{"id": "main", "name": "Main", "kind": "generic", "endpoint": f.registryURL, "username": "bot", "password": "pw-api-secret"}
	wantStatus(t, admin.do("POST", "/api/v1/registries", reg), http.StatusCreated)

	// Catalog: every role that may read image builds; anonymous callers cannot.
	var cat imagecompose.Catalog
	r := viewer.do("GET", "/api/v1/image-composer/catalog", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &cat)
	if cat.Evidence != "unverified" || len(cat.Components) < 3 || len(cat.Distros) < 4 {
		t.Fatalf("%+v", cat)
	}
	wantError(t, f.client().do("GET", "/api/v1/image-composer/catalog", nil), http.StatusUnauthorized, errUnauthenticated)

	// Preview: operators and administrators; viewers are denied and audited.
	body := map[string]any{"recipe": composerRecipe()}
	wantError(t, viewer.do("POST", "/api/v1/image-composer/preview", body), http.StatusForbidden, errForbidden)
	if auditsOf(f.fixture, authz.ActionCreate, authz.OutcomeDenied, authz.ResourceImageBuild) != 1 {
		t.Fatal("denied preview not audited")
	}
	var pv ComposerPreview
	r = op.do("POST", "/api/v1/image-composer/preview", body)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &pv)
	if !pv.Valid || pv.Digest == "" || !strings.Contains(pv.Dockerfile, "FROM docker.io/library/alpine:3.20@sha256:") ||
		pv.Recipe == nil || len(pv.Files) == 0 || len(pv.Packages) == 0 || len(pv.Variants) != 9 || len(pv.Warnings) == 0 {
		t.Fatalf("%+v", pv)
	}
	if len(f.builder.Calls) != 0 {
		t.Fatal("preview built something")
	}
	// An invalid recipe is a 200 with findings, so a form can show them.
	bad := composerRecipe()
	bad["components"].([]any)[1].(map[string]any)["settings"] = map[string]any{"auto_prepend_file": "/x"}
	r = op.do("POST", "/api/v1/image-composer/preview", map[string]any{"recipe": bad})
	wantStatus(t, r, http.StatusOK)
	var inv ComposerPreview
	r.decode(t, &inv)
	if inv.Valid || len(inv.Issues) == 0 || inv.Dockerfile != "" {
		t.Fatalf("%+v", inv)
	}
	wantError(t, op.do("POST", "/api/v1/image-composer/preview", map[string]any{"recipe": 1}), http.StatusBadRequest, errBadRequest)

	// Build: administrators only (operators may preview but not build).
	build := map[string]any{"recipe": composerRecipe(), "target": map[string]any{"registry": "main", "repository": "team/shop", "tag": "1"}}
	wantError(t, op.do("POST", "/api/v1/image-composer/builds", build), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("POST", "/api/v1/image-composer/builds", build), http.StatusForbidden, errForbidden)
	var res ImageBuildResult
	r = admin.do("POST", "/api/v1/image-composer/builds", build)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if res.Plan == nil || res.Job != nil || res.Plan.SpecHash != pv.Digest || res.Plan.ContextHash != pv.ContextHash || len(f.builder.Calls) != 0 {
		t.Fatalf("%+v", res.Plan)
	}
	// Approval: a wrong digest is refused, a recipe that changed after the
	// review needs a new approval, the right digest builds.
	build["approvalDigest"] = "sha256:" + strings.Repeat("0", 64)
	wantError(t, admin.do("POST", "/api/v1/image-composer/builds", build), http.StatusConflict, errApprovalMismatch)
	build["approvalDigest"] = res.Plan.Digest
	changed := composerRecipe()
	changed["components"].([]any)[1].(map[string]any)["settings"] = map[string]any{"memory_limit": "512M"}
	build["recipe"] = changed
	wantError(t, admin.do("POST", "/api/v1/image-composer/builds", build), http.StatusConflict, errApprovalMismatch)
	if len(f.builder.Calls) != 0 {
		t.Fatal("unapproved build ran")
	}
	build["recipe"] = composerRecipe()
	r = admin.do("POST", "/api/v1/image-composer/builds", build)
	wantStatus(t, r, http.StatusAccepted)
	var started ImageBuildResult
	r.decode(t, &started)
	if started.Job == nil || started.Job.Status != imagebuild.JobSucceeded || started.Job.Source != "composer" ||
		started.Job.SpecHash != pv.Digest || len(started.Job.Recipe) == 0 || len(f.builder.Calls) != 1 {
		t.Fatalf("%+v", started.Job)
	}
	if strings.Contains(r.Body.String(), "pw-api-secret") {
		t.Fatal("password in job response")
	}
	// The built context is the generated one: non-root, digest-pinned.
	in := f.builder.Calls[0]
	if !strings.Contains(in.Rendered.Dockerfile, "USER 10001:10001") || in.Rendered.SpecHash != pv.Digest {
		t.Fatalf("%s", in.Rendered.Dockerfile)
	}
	// Invalid recipes and unknown registries are client errors.
	build["approvalDigest"] = ""
	build["recipe"] = bad
	wantError(t, admin.do("POST", "/api/v1/image-composer/builds", build), http.StatusBadRequest, errBadRequest)
	build["recipe"] = composerRecipe()
	build["target"] = map[string]any{"registry": "nope", "repository": "a", "tag": "1"}
	wantError(t, admin.do("POST", "/api/v1/image-composer/builds", build), http.StatusNotFound, errNotFound)
}

func TestImageComposerUnavailableWithoutService(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/image-composer/catalog", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("POST", "/api/v1/image-composer/preview", map[string]any{"recipe": map[string]any{}}), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("POST", "/api/v1/image-composer/builds", map[string]any{"recipe": map[string]any{}}), http.StatusServiceUnavailable, errUnavailable)
}
