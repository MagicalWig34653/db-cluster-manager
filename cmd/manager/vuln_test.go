// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

const mirrorRecord = `[{
  "id": "GHSA-web1-test-0001",
  "aliases": ["CVE-2026-100001"],
  "summary": "web image flaw",
  "modified": "2026-10-23T08:00:00Z",
  "published": "2026-10-22T08:00:00Z",
  "database_specific": {"severity": "HIGH"},
  "affected": [{
    "package": {"ecosystem": "docker", "name": "team/web"},
    "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.2.5"}]}]
  }]
}]`

func vulnRuntime(t *testing.T, section map[string]any) (*managedRuntime, error) {
	t.Helper()
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t), "vuln": section},
	}))
	if err != nil {
		return nil, err
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = rt.Close() })
	waitUntil(t, "vulnerability service", func() bool { return rt.vuln.Load() != nil })
	return rt, nil
}

func TestManagedModeWiresVulnerabilityResponse(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "mirror.json")
	keyFile := filepath.Join(dir, "provider.key")
	const key = "sk-test-provider-key-for-wiring-0123456789"
	if err := os.WriteFile(feed, []byte(mirrorRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := vulnRuntime(t, map[string]any{
		"defaultMode": "approval_required",
		"feeds":       []map[string]any{{"kind": "osvFile", "path": feed}},
		"providers": []map[string]any{{"id": "local", "name": "Local", "kind": "claude_api", "models": []string{"claude-test"},
			"defaultModel": "claude-test", "apiKeyFile": keyFile}},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := rt.vuln.Load()
	st := rt.svc.Store()
	if _, err := manager.Put(st, manager.CollServices, "s1", manager.ServiceRecord{ID: "s1", Swarm: "sw", Name: "shop", Type: "container",
		Spec: json.RawMessage(`{"image":"reg.example.test:5000/team/web:1.2.0"}`)}, nil); err != nil {
		t.Fatal(err)
	}

	// The scheduler's poll function: feed -> inventory -> finding.
	if err := rt.pollVuln(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs, err := svc.Findings()
	if err != nil || len(fs) != 1 || fs[0].AdvisoryID == "" || fs[0].Component.Name != "team/web" || len(fs[0].Services) != 1 || fs[0].Services[0] != "sw/shop" {
		t.Fatalf("findings: %+v %v", fs, err)
	}
	recs, _ := svc.Audit()
	var polls int
	for _, r := range recs {
		if r.Event == vuln.EventPoll {
			polls++
		}
	}
	if polls != 1 {
		t.Fatalf("poll audit records: %d", polls)
	}

	// Seeded defaults: approval_required, provider registered without
	// consent, the key sealed and not in the clear.
	if p, _, _ := svc.Policy(); p.Mode != vuln.ModeApprovalRequired {
		t.Fatalf("policy: %+v", p)
	}
	provs, err := svc.Providers()
	if err != nil || len(provs) != 1 || provs[0].ID != "local" || provs[0].Consent != nil {
		t.Fatalf("providers: %+v %v", provs, err)
	}
	raw, ok, _ := st.Raw(manager.CollAISecrets, "vuln/ai/local/key")
	if !ok || bytes.Contains(raw.Value, []byte(key)) {
		t.Fatal("provider key missing or stored in clear")
	}
	if feeds, _ := svc.FeedStates(); len(feeds) != 1 || feeds[0].LastError != "" {
		t.Fatalf("feeds: %+v", feeds)
	}
	// The routes are mounted (session required).
	if rec := serve(t, newMux(rt), http.MethodGet, "/api/v1/vuln/findings", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /vuln/findings = %d, want 401", rec.Code)
	}
}

func TestVulnDefaultsStayQuiet(t *testing.T) {
	rt, err := vulnRuntime(t, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	svc := rt.vuln.Load()
	if p, _, _ := svc.Policy(); p.Mode != vuln.ModeNotifyOnly {
		t.Fatalf("default mode: %q", p.Mode)
	}
	if feeds, _ := svc.FeedStates(); len(feeds) != 0 {
		t.Fatalf("no feed may exist without configuration: %+v", feeds)
	}
	if ps, _ := svc.Providers(); len(ps) != 0 {
		t.Fatalf("no provider without configuration: %+v", ps)
	}
	if err := rt.pollVuln(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVulnConfigValidation(t *testing.T) {
	cases := map[string]vulnConfig{
		"interval":      {PollIntervalSeconds: 10},
		"mode":          {DefaultMode: "auto_for_policy"},
		"no url":        {Feeds: []vulnFeedConfig{{Kind: "osv"}}},
		"http remote":   {Feeds: []vulnFeedConfig{{Kind: "osv", URL: "http://api.osv.dev"}}},
		"credentials":   {Feeds: []vulnFeedConfig{{Kind: "kev", URL: "https://u:p@example.org/kev.json"}}},
		"duplicate":     {Feeds: []vulnFeedConfig{{Kind: "osv", URL: "https://a.example"}, {Kind: "osv", URL: "https://b.example"}}},
		"unknown kind":  {Feeds: []vulnFeedConfig{{Kind: "rss", URL: "https://a.example"}}},
		"relative file": {Feeds: []vulnFeedConfig{{Kind: "osvFile", Path: "mirror.json"}}},
		"osv key":       {Feeds: []vulnFeedConfig{{Kind: "osv", URL: "https://a.example", APIKeyFile: "/run/secrets/k"}}},
		"cli provider":  {Providers: []vulnProviderConfig{{ID: "c", Name: "c", Kind: ai.KindClaudeCodeCLI}}},
		"bad provider":  {Providers: []vulnProviderConfig{{ID: "", Kind: ai.KindClaudeAPI}}},
	}
	for name, c := range cases {
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := vulnConfig{
		Feeds: []vulnFeedConfig{{Kind: "osv", URL: "https://api.osv.dev"}, {Kind: "kev", URL: "https://example.org/kev.json"},
			{Kind: "github", URL: "http://127.0.0.1:9", APIKeyFile: "/run/secrets/gh"}},
		Providers: []vulnProviderConfig{{ID: "p", Name: "P", Kind: ai.KindClaudeAPI, APIKeyFile: "/run/secrets/k"}},
	}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVulnNeedsAuth(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"manager": map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t), "vuln": map[string]any{}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "auth.enabled") {
		t.Fatalf("manager.vuln without auth: %v", err)
	}
}
