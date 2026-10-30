// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/modules"
)

type memModuleStore map[string]modules.State

func (m memModuleStore) Load() (map[string]modules.State, error) { return m, nil }
func (m memModuleStore) Save(id string, s modules.State) error   { m[id] = s; return nil }

// useModules installs a registry; every module starts off.
func (f *fixture) useModules(t *testing.T, missing func(string) []string) *modules.Registry {
	t.Helper()
	reg, err := modules.New(memModuleStore{}, nil, missing)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Modules = reg
	return reg
}

func TestModuleSwitchAndRoutes(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.Global)
	f.useModules(t, func(id string) []string {
		if id == "image-build" {
			return []string{"manager.imageBuild"}
		}
		return nil
	})

	// A disabled module's routes answer uniformly, before any backend.
	wantError(t, admin.do("GET", "/api/v1/patch/hosts", nil), http.StatusNotFound, errModuleDisabled)
	wantError(t, admin.do("POST", "/api/v1/clusters/c1/backup-copies", map[string]string{}), http.StatusNotFound, errModuleDisabled)

	// Any session lists; only an administrator switches.
	wantStatus(t, viewer.do("GET", "/api/v1/modules", nil), http.StatusOK)
	wantError(t, viewer.do("PUT", "/api/v1/modules/vuln", map[string]bool{"enabled": true}), http.StatusForbidden, errForbidden)

	wantStatus(t, admin.do("PUT", "/api/v1/modules/vuln", map[string]bool{"enabled": true}), http.StatusOK)
	if r := admin.do("GET", "/api/v1/vuln/findings", nil); r.Code == http.StatusNotFound {
		t.Fatalf("enabled module still refused: %s", r.Body)
	}
	wantError(t, admin.do("PUT", "/api/v1/modules/image-build", map[string]bool{"enabled": true}), http.StatusConflict, errModuleBlocked)
	wantError(t, admin.do("PUT", "/api/v1/modules/image-composer", map[string]bool{"enabled": true}), http.StatusConflict, errModuleBlocked)
	wantError(t, admin.do("PUT", "/api/v1/modules/nope", map[string]bool{"enabled": true}), http.StatusNotFound, errNotFound)

	var list List[modules.Info]
	admin.do("GET", "/api/v1/modules", nil).decode(t, &list)
	for _, m := range list.Items {
		if m.ID == "image-composer" && (m.Enabled || len(m.MissingModules) != 1) {
			t.Fatalf("composer: %+v", m)
		}
	}

	wantStatus(t, admin.do("PUT", "/api/v1/modules/vuln", map[string]bool{"enabled": false}), http.StatusOK)
	wantError(t, admin.do("GET", "/api/v1/vuln/findings", nil), http.StatusNotFound, errModuleDisabled)
}
