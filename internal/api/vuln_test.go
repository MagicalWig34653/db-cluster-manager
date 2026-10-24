// SPDX-License-Identifier: AGPL-3.0-only

package api

// Fakes only: an offline OSV mirror, a static inventory and the in-memory
// KV behind a real vuln.Service. No feed, provider or build is contacted.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

const vulnMirror = `[{"id":"CVE-2026-109618","summary":"PHP upload progress leak","database_specific":{"severity":"CRITICAL"},
 "affected":[{"package":{"ecosystem":"Alpine:v3.20","name":"php83"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"8.3.12-r0"}]}]}]}]`

func newVulnFixture(t *testing.T) (*fixture, *vuln.Service) {
	f := newFixture(t)
	svc := &vuln.Service{
		Store: &vuln.Store{KV: &vuln.MemoryKV{}},
		Inventory: &vuln.StaticInventory{Entries: []vuln.InventoryEntry{{
			ID: "img-shop", Image: "vendor/shop:1", Services: []string{"shop-web"}, ServiceKind: vuln.ServiceKindWeb, Coverage: vuln.CoverageSBOM,
			Components: []vuln.Component{{Ecosystem: "Alpine:v3.20", Name: "php83", Version: "8.3.10-r0"}},
		}}},
		Feeds:   []vuln.Feed{&vuln.OfflineFeed{Label: "mirror", Load: func() ([]byte, error) { return []byte(vulnMirror), nil }}},
		Secrets: &ai.MemorySecretStore{}, Budget: &ai.Budget{},
	}
	f.srv.cfg.Vuln = &VulnBackend{Svc: svc}
	return f, svc
}

func TestVulnRoutesDenyByDefault(t *testing.T) {
	f, _ := newVulnFixture(t)
	f.setupAdmin("admin")
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	dba := f.loggedIn("dba", authz.RoleDatabaseAdmin, authz.Global)
	for _, c := range []*client{viewer, op, dba} {
		for _, r := range []struct{ m, p string }{
			{"GET", "/vuln/findings"}, {"GET", "/vuln/findings/x"}, {"PUT", "/vuln/findings/x/status"}, {"POST", "/vuln/findings/x/proposals"},
			{"GET", "/vuln/proposals"}, {"POST", "/vuln/proposals/x/approve"}, {"POST", "/vuln/proposals/x/decline"},
			{"GET", "/vuln/feeds"}, {"POST", "/vuln/poll"}, {"GET", "/vuln/policy"}, {"PUT", "/vuln/policy"},
			{"GET", "/vuln/providers"}, {"POST", "/vuln/providers"}, {"PUT", "/vuln/providers/x/consent"}, {"GET", "/vuln/audit"},
		} {
			wantError(t, c.do(r.m, "/api/v1"+r.p, map[string]any{}), http.StatusForbidden, errForbidden)
		}
	}
}

func TestVulnRoutesEndToEnd(t *testing.T) {
	f, svc := newVulnFixture(t)
	admin, _, _ := f.setupAdmin("admin")

	var rep vuln.PollReport
	r := admin.do("POST", "/api/v1/vuln/poll", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &rep)
	if len(rep.NewFindings) != 1 {
		t.Fatalf("%+v", rep)
	}
	var fs []vuln.Finding
	admin.do("GET", "/api/v1/vuln/findings", nil).decode(t, &fs)
	if len(fs) != 1 || fs[0].AdvisoryID != "CVE-2026-109618" {
		t.Fatalf("%+v", fs)
	}
	wantError(t, admin.do("GET", "/api/v1/vuln/findings/nope", nil), http.StatusNotFound, errNotFound)

	// Notify-only default: a proposal exists but cannot be approved.
	var p vuln.Proposal
	r = admin.do("POST", "/api/v1/vuln/findings/"+fs[0].ID+"/proposals", nil)
	wantStatus(t, r, http.StatusCreated)
	r.decode(t, &p)
	if p.Kind != vuln.KindPatchedRebuild || !p.Plan.Manual || p.Status != vuln.StatusProposed {
		t.Fatalf("%+v", p)
	}
	wantError(t, admin.do("POST", "/api/v1/vuln/proposals/"+p.ID+"/approve", map[string]any{"digest": p.Plan.Digest}), http.StatusConflict, errPolicyConflict)

	var pv VulnPolicyView
	admin.do("GET", "/api/v1/vuln/policy", nil).decode(t, &pv)
	if pv.Policy.Mode != vuln.ModeNotifyOnly || pv.Version != 0 {
		t.Fatalf("%+v", pv)
	}
	wantError(t, admin.do("PUT", "/api/v1/vuln/policy", map[string]any{"policy": map[string]any{"mode": "auto_for_policy"}, "version": 0}), http.StatusBadRequest, errBadRequest)
	wantStatus(t, admin.do("PUT", "/api/v1/vuln/policy", map[string]any{"policy": map[string]any{"mode": "approval_required"}, "version": 0}), http.StatusOK)
	wantError(t, admin.do("PUT", "/api/v1/vuln/policy", map[string]any{"policy": map[string]any{"mode": "approval_required"}, "version": 0}), http.StatusConflict, errConflict)

	admin.do("POST", "/api/v1/vuln/findings/"+fs[0].ID+"/proposals", map[string]any{"useAi": false}).decode(t, &p)
	if p.Status != vuln.StatusAwaitingApproval {
		t.Fatal(p.Status)
	}
	wantError(t, admin.do("POST", "/api/v1/vuln/proposals/"+p.ID+"/approve", map[string]any{"digest": "sha256:x"}), http.StatusConflict, errApprovalMismatch)
	wantError(t, admin.do("POST", "/api/v1/vuln/proposals/"+p.ID+"/approve", map[string]any{}), http.StatusBadRequest, errBadRequest)
	r = admin.do("POST", "/api/v1/vuln/proposals/"+p.ID+"/approve", map[string]any{"digest": p.Plan.Digest})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &p)
	if p.Status != vuln.StatusApproved || !strings.Contains(p.Execution, "no executor") {
		t.Fatalf("%+v", p)
	}
	wantError(t, admin.do("POST", "/api/v1/vuln/proposals/"+p.ID+"/approve", map[string]any{"digest": p.Plan.Digest}), http.StatusConflict, errConflict)

	// AI analysis without a provider in the policy is a bad request.
	wantError(t, admin.do("POST", "/api/v1/vuln/findings/"+fs[0].ID+"/proposals", map[string]any{"useAi": true}), http.StatusBadRequest, errBadRequest)

	// Provider registration: the key is accepted once, never returned.
	prov := map[string]any{"id": "claude", "name": "Claude", "kind": "claude_api", "models": []string{"claude-x"}, "apiKey": "sk-ant-api-secret-0123456789"}
	r = admin.do("POST", "/api/v1/vuln/providers", prov)
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "sk-ant-api-secret") || strings.Contains(r.Body.String(), "vuln/ai/") {
		t.Fatal("key or reference in response")
	}
	wantError(t, admin.do("POST", "/api/v1/vuln/providers", prov), http.StatusConflict, errConflict)
	wantError(t, admin.do("POST", "/api/v1/vuln/providers", map[string]any{"id": "x", "name": "x", "kind": "openai_compatible", "baseUrl": "http://models.example.com/v1"}), http.StatusBadRequest, errBadRequest)
	var views []vuln.ProviderView
	r = admin.do("GET", "/api/v1/vuln/providers", nil)
	r.decode(t, &views)
	if len(views) != 1 || !views[0].CredentialSet || views[0].Consent != nil || strings.Contains(r.Body.String(), "sk-ant") {
		t.Fatalf("%s", r.Body.String())
	}
	admin.do("GET", "/api/v1/vuln/policy", nil).decode(t, &pv)
	wantStatus(t, admin.do("PUT", "/api/v1/vuln/policy", map[string]any{"policy": map[string]any{"mode": "approval_required", "provider": "claude", "model": "claude-x"}, "version": pv.Version}), http.StatusOK)
	// Not consented: 409 settings_incomplete, nothing sent.
	wantError(t, admin.do("POST", "/api/v1/vuln/findings/"+fs[0].ID+"/proposals", map[string]any{"useAi": true}), http.StatusConflict, errSettingsIncomplete)
	wantError(t, admin.do("PUT", "/api/v1/vuln/providers/claude/consent", map[string]any{"grant": true, "statement": "yes"}), http.StatusConflict, errApprovalMismatch)
	wantStatus(t, admin.do("PUT", "/api/v1/vuln/providers/claude/consent", map[string]any{"grant": true, "statement": views[0].Egress}), http.StatusOK)

	// VEX status and audit.
	wantError(t, admin.do("PUT", "/api/v1/vuln/findings/"+fs[0].ID+"/status", map[string]any{"status": "not_affected"}), http.StatusBadRequest, errBadRequest)
	wantStatus(t, admin.do("PUT", "/api/v1/vuln/findings/"+fs[0].ID+"/status", map[string]any{"status": "under_investigation"}), http.StatusOK)
	var recs []vuln.AuditRecord
	r = admin.do("GET", "/api/v1/vuln/audit", nil)
	r.decode(t, &recs)
	if len(recs) < 8 || strings.Contains(r.Body.String(), "sk-ant-api-secret") {
		t.Fatalf("%d records", len(recs))
	}
	var feeds []vuln.FeedState
	admin.do("GET", "/api/v1/vuln/feeds", nil).decode(t, &feeds)
	if len(feeds) != 1 || feeds[0].Stale {
		t.Fatalf("%+v", feeds)
	}
	if s, err := svc.Secrets.Get(context.Background(), "vuln/ai/claude/key"); err != nil || s.Reveal() != "sk-ant-api-secret-0123456789" {
		t.Fatal("key must be in the secret store", err)
	}
}

func TestVulnRoutesUnavailableWhenUnwired(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/vuln/findings", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("POST", "/api/v1/vuln/poll", nil), http.StatusServiceUnavailable, errUnavailable)
}
