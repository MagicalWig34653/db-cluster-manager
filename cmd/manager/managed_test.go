// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

func freeLoopback(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestManagedModeKeepsAdministratorAcrossRestart starts the manager with a
// manager section (single-instance mode, loopback metastore, data on disk),
// creates an administrator in the replicated auth state, restarts it and
// checks that setup is closed and no new setup token is printed.
func TestManagedModeKeepsAdministratorAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	bind := freeLoopback(t)
	p := writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": map[string]any{"dataDir": dir, "bind": bind},
	})
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rt, err := newManagedRuntime(cfg, quietLog(), &out)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(rt)
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	if rec := serve(t, mux, "GET", "/api/v1/setup/status", ""); !strings.Contains(rec.Body.String(), `"setupRequired":true`) {
		t.Fatalf("setup status %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(out.String(), "setup token") {
		t.Fatal("no setup token printed on a fresh installation")
	}
	// The resource endpoints are backed by the store now (401 without a
	// session instead of 503).
	if rec := serve(t, mux, "GET", "/api/v1/swarms", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("swarms = %d", rec.Code)
	}
	st := rt.stack.Load()
	if err := st.Accounts.Create(recovery.Account{ID: "admin", Credentials: []recovery.Credential{{ID: "pk", Kind: recovery.CredentialPasskey}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Policy.Bind(authz.Binding{Subject: authz.Subject{ID: "admin", Kind: authz.SubjectUser}, Role: authz.RoleAdministrator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	rt.prune()
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	rt, err = newManagedRuntime(cfg, quietLog(), &out)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mux = newMux(rt)
	waitUntil(t, "leader API after restart", func() bool { return rt.stack.Load() != nil })
	if rec := serve(t, mux, "GET", "/api/v1/setup/status", ""); !strings.Contains(rec.Body.String(), `"setupRequired":false`) {
		t.Fatalf("setup reopened after restart: %s", rec.Body.String())
	}
	if out.Len() != 0 {
		t.Fatalf("setup token printed although an administrator exists: %q", out.String())
	}
}

func TestManagedModeNotLeaderAnswers503(t *testing.T) {
	rt := &managedRuntime{proto: newProto()}
	rt.setHandler(rt.notLeader())
	if rec := serve(t, rt, "GET", "/api/v1/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	if rec := serve(t, rt, "GET", "/api/v1/swarms", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("swarms on non-leader = %d", rec.Code)
	}
}

func TestManagerSectionValidation(t *testing.T) {
	for _, m := range []map[string]any{{"bind": "127.0.0.1:0"}, {"dataDir": t.TempDir()}} {
		cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": m}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", m)
		}
	}
	if _, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"dataDirectory": "x"}})); err == nil {
		t.Fatal("unknown manager field accepted")
	}
}

func newProto() http.Handler { return api.NewHandler() }
