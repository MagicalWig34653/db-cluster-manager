// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// wiringRuntime starts a single managed runtime with auth enabled and the
// given manager sections, waits for the leader API and returns it.
func wiringRuntime(t *testing.T, sections map[string]any, withTransport bool) (*managedRuntime, error) {
	t.Helper()
	mgr := map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t)}
	if withTransport {
		enrollAddr := freeLoopback(t)
		mgr["agentTransport"] = map[string]any{"enrollListen": enrollAddr, "managerUrl": "https://" + enrollAddr}
	}
	for k, v := range sections {
		mgr[k] = v
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
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	return rt, nil
}

// sessionFor creates an account with the role and returns its session token.
func sessionFor(t *testing.T, rt *managedRuntime, id, role string) string {
	t.Helper()
	st := rt.stack.Load()
	if err := st.Accounts.Create(recovery.Account{ID: id, Credentials: []recovery.Credential{{ID: "pk-" + id, Kind: recovery.CredentialPasskey}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Policy.Bind(authz.Binding{Subject: authz.Subject{ID: id, Kind: authz.SubjectUser}, Role: role, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	token, _, err := st.Sessions.Issue(id)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// call sends one authenticated API request through the runtime.
func call(t *testing.T, rt *managedRuntime, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewBufferString(body))
	req.Header.Set("Origin", "https://db.example.test")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-dcm_session", Value: token})
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return rec
}
