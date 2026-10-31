// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/modules"
)

// An enabled module that needs another one blocks switching that one off:
// 409 module_blocked, nothing changes; after the dependent is off it works.
func TestModuleDisableBlockedByDependents(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	f.useModules(t, func(string) []string { return nil })
	wantStatus(t, admin.do("PUT", "/api/v1/modules/image-build", map[string]bool{"enabled": true}), http.StatusOK)
	wantStatus(t, admin.do("PUT", "/api/v1/modules/image-composer", map[string]bool{"enabled": true}), http.StatusOK)

	wantError(t, admin.do("PUT", "/api/v1/modules/image-build", map[string]bool{"enabled": false}), http.StatusConflict, errModuleBlocked)
	var list List[modules.Info]
	admin.do("GET", "/api/v1/modules", nil).decode(t, &list)
	found := false
	for _, m := range list.Items {
		if m.ID == "image-build" {
			found = true
			if !m.Enabled || len(m.Dependents) != 1 || m.Dependents[0] != "image-composer" {
				t.Fatalf("image-build after refused disable: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("image-build not listed")
	}

	wantStatus(t, admin.do("PUT", "/api/v1/modules/image-composer", map[string]bool{"enabled": false}), http.StatusOK)
	wantStatus(t, admin.do("PUT", "/api/v1/modules/image-build", map[string]bool{"enabled": false}), http.StatusOK)
}
