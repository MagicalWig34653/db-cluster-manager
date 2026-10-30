// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// A module is switched on the running leader through the API; the state is
// stored in the metastore and the configuration section only seeded it.
func TestModuleSwitchAtRuntime(t *testing.T) {
	rt, err := wiringRuntime(t, map[string]any{}, true)
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
	put := func(id, body string, want int) {
		t.Helper()
		if rec := call(t, rt, admin, "PUT", "/modules/"+id, body); rec.Code != want {
			t.Fatalf("PUT %s %s = %d (%s), want %d", id, body, rec.Code, rec.Body, want)
		}
	}
	if rt.on("image-update") {
		t.Fatal("on without configuration or switch")
	}
	put("image-update", `{"enabled":true}`, http.StatusOK)
	if !rt.on("image-update") {
		t.Fatal("switch not applied without restart")
	}
	put("image-composer", `{"enabled":true}`, http.StatusConflict) // needs image-build and its configuration
	put("image-update", `{"enabled":false}`, http.StatusOK)
	if rt.on("image-update") {
		t.Fatal("still running after disable")
	}
	if _, ok, err := rt.svc.Store().Raw(collModules, "image-update"); err != nil || !ok {
		t.Fatalf("state not stored: %v", err)
	}
}
