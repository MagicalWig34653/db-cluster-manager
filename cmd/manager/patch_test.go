// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

func patchRuntime(t *testing.T, section any, withTransport bool) (*managedRuntime, error) {
	t.Helper()
	mgr := map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t)}
	if withTransport {
		enrollAddr := freeLoopback(t)
		mgr["agentTransport"] = map[string]any{"enrollListen": enrollAddr, "managerUrl": "https://" + enrollAddr}
	}
	if section != nil {
		mgr["patch"] = section
	}
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": mgr,
	}))
	if err != nil {
		return nil, err
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = rt.Close() })
	waitUntil(t, "leadership", rt.svc.IsLeader)
	return rt, nil
}

func TestPatchIsOffByDefault(t *testing.T) {
	for name, section := range map[string]any{"absent": nil, "disabled": map[string]any{"enabled": false}} {
		t.Run(name, func(t *testing.T) {
			rt, err := patchRuntime(t, section, true)
			if err != nil {
				t.Fatal(err)
			}
			// The API is built on leadership; wait until it serves.
			waitUntil(t, "api", func() bool { return rt.stack.Load() != nil })
			if rt.patch.Load() != nil {
				t.Fatal("patch service is running although it was not enabled")
			}
			// The routes exist (session required) and are not served by a backend.
			if rec := serve(t, newMux(rt), http.MethodGet, "/api/v1/patch/hosts", ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /patch/hosts = %d, want 401", rec.Code)
			}
		})
	}
}

func TestPatchEnabledWiresRealGuardAndStore(t *testing.T) {
	rt, err := patchRuntime(t, map[string]any{"enabled": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "patch service", func() bool { return rt.patch.Load() != nil })
	svc := rt.patch.Load()
	if _, ok := svc.Guard.(patch.GaleraGuard); !ok {
		t.Fatalf("guard %T: the Galera guard must be the real planner adapter, not a fake", svc.Guard)
	}
	if svc.Journal == nil || svc.Term == nil || svc.DenyList == nil || svc.Agents == nil {
		t.Fatalf("incomplete wiring: %+v", svc)
	}
	if _, err := svc.Term(); err != nil {
		t.Fatalf("leader term: %v", err)
	}
	// Metastore-backed: records written through the service are in the
	// replicated store and the default policy is report-only.
	if _, err := svc.UpsertHost(patch.Host{ID: "h1"}); err != nil {
		t.Fatal(err)
	}
	if hs, err := svc.Hosts(); err != nil || len(hs) != 1 {
		t.Fatal(hs, err)
	}
	if pol, err := svc.DefaultPolicy(); err != nil || pol.Mode != patch.ModeReportOnly {
		t.Fatalf("default policy %+v %v", pol, err)
	}
	if _, ok, _ := rt.svc.Store().Raw(patch.CollHosts, "h1"); !ok {
		t.Fatal("host record is not in the metastore")
	}
	// A deny list can be built from the replicated revocations.
	if dl, err := svc.DenyList(); err != nil || dl.Digest == "" {
		t.Fatalf("deny list %+v %v", dl, err)
	}
}

func TestPatchConfigValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		section   any
		transport bool
		want      string
	}{
		"needs agent transport": {map[string]any{"enabled": true}, false, "agentTransport"},
		"poll too fast":         {map[string]any{"enabled": true, "pollIntervalSeconds": 5}, true, "pollIntervalSeconds"},
		"bad port":              {map[string]any{"enabled": true, "agentPort": 70000}, true, "agentPort"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := patchRuntime(t, tc.section, tc.transport)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
	// A disabled section is not validated against the transport: off is off.
	if _, err := patchRuntime(t, map[string]any{"enabled": false}, false); err != nil {
		t.Fatal(err)
	}
}
