// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serve(t *testing.T, h http.Handler, method, path, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func writeConfig(t *testing.T, v any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultIsPrototype(t *testing.T) {
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	h, st, err := newAPIHandler(cfg, quietLog(), &out)
	if err != nil || st != nil {
		t.Fatalf("err=%v stores=%v", err, st)
	}
	mux := newMux(h)
	if rec := serve(t, mux, "GET", "/api/v1/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	if rec := serve(t, mux, "GET", "/api/v1/version", ""); rec.Code != http.StatusOK {
		t.Fatalf("version = %d", rec.Code)
	}
	if rec := serve(t, mux, "GET", "/api/v1/setup/status", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("setup must not exist in prototype mode: %d", rec.Code)
	}
	// The UI route is still mounted (503 while no UI is built, 200 otherwise).
	if rec := serve(t, mux, "GET", "/", ""); rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ui = %d", rec.Code)
	}
	if out.Len() != 0 {
		t.Fatal("no setup token may be printed in prototype mode")
	}
}

func TestAuthConfigMountsFullAPI(t *testing.T) {
	p := writeConfig(t, map[string]any{"auth": map[string]any{
		"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"},
	}})
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	h, st, err := newAPIHandler(cfg, quietLog(), &out)
	if err != nil || st == nil {
		t.Fatalf("err=%v", err)
	}
	st.prune()
	if !strings.Contains(out.String(), "setup token") {
		t.Fatalf("setup token not printed: %q", out.String())
	}
	mux := newMux(h)
	rec := serve(t, mux, "GET", "/api/v1/setup/status", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"setupRequired":true`) {
		t.Fatalf("setup status = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(t, mux, "GET", "/api/v1/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	if rec := serve(t, mux, "GET", "/api/v1/swarms", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("swarms without session = %d", rec.Code)
	}
	if rec := serve(t, mux, "POST", "/api/v1/auth/login/begin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin = %d", rec.Code)
	}
	if rec := serve(t, mux, "POST", "/api/v1/auth/login/begin", "https://db.example.test"); rec.Code != http.StatusOK {
		t.Fatalf("login begin = %d %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidConfigsAreRejected(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file must fail")
	}
	p := writeConfig(t, map[string]any{"auth": map[string]any{"enabled": true, "relyingParty": "typo"}})
	if _, err := loadConfig(p); err == nil {
		t.Fatal("unknown field must fail")
	}
	p = writeConfig(t, map[string]any{"auth": map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"http://db.example.test"}}})
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := newAPIHandler(cfg, quietLog(), io.Discard); err == nil {
		t.Fatal("plain-http origin must be rejected")
	}
}

func TestConfigFromEnvironment(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	env := func(k string) string {
		if k == configEnv {
			return missing
		}
		return ""
	}
	// The environment supplies the -config default, so loading fails
	// before the server listens.
	err := run([]string{"-listen", "127.0.0.1:0"}, env, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("err = %v, want config read error from $%s", err, configEnv)
	}
	if err := run([]string{"-bogus"}, env, io.Discard); err == nil {
		t.Fatal("unknown flag must fail")
	}
}
