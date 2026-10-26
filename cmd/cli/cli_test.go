// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICommandsAgainstAPI(t *testing.T) {
	var calls []string
	base := "https://manager.example"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("__Host-dcm_session"); err != nil || c.Value != "fake-session" {
			http.Error(w, `{"error":"unauthenticated"}`, 401)
			return
		}
		if r.Method != "GET" && r.Header.Get("Origin") != base {
			http.Error(w, `{"error":"origin_rejected"}`, 403)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/session":
			w.Write([]byte(`{"accountId":"admin"}`))
		case "/api/v1/clusters":
			if r.Method == "POST" {
				w.WriteHeader(202)
				w.Write([]byte(`{"id":"create1"}`))
			} else {
				w.Write([]byte(`{"items":[{"id":"c1","name":"demo"}]}`))
			}
		case "/api/v1/clusters/c1":
			if r.Method == "DELETE" {
				w.WriteHeader(202)
				w.Write([]byte(`{"id":"delete1","approval":{"digest":"abc"}}`))
			} else {
				w.Write([]byte(`{"id":"c1"}`))
			}
		case "/api/v1/clusters/c1/operation-plan":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"plan1","digest":"abc"}`))
		case "/api/v1/operations":
			w.Write([]byte(`{"items":[{"id":"op1","state":"succeeded"}]}`))
		case "/api/v1/operations/op1/approve":
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v["digest"] != "abc" {
				t.Errorf("digest = %q", v["digest"])
			}
			w.Write([]byte(`{"digest":"abc"}`))
		case "/api/v1/operations/op1/abort":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"op1"}`))
		case "/api/v1/backups":
			w.Write([]byte(`{"items":[{"cluster":"c1","lastVerifiedRestoreAt":"2026-10-26T00:00:00Z"}]}`))
		case "/api/v1/clusters/c1/backups":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"backup1"}`))
		case "/api/v1/hosts":
			w.Write([]byte(`{"items":[{"nodeId":"h1"}]}`))
		case "/api/v1/services/plan":
			if r.URL.Query().Get("format") == "yaml" {
				w.Header().Set("Content-Type", "application/yaml")
				w.Write([]byte("digest: abc\n"))
			} else {
				w.Write([]byte(`{"digest":"abc"}`))
			}
		case "/api/v1/services":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"service1"}`))
		case "/api/v1/restores/inspect":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"inspect1","inspectionPassword":"private-value","approval":{"digest":"abc"}}`))
		case "/api/v1/vuln/findings":
			w.Write([]byte(`[{"id":"f1"}]`))
		case "/api/v1/vuln/proposals/p1/approve":
			w.Write([]byte(`{"id":"p1"}`))
		default:
			http.NotFound(w, r)
		}
	})
	dir := t.TempDir()
	configFile := filepath.Join(dir, "cli.json")
	cfg := config{Current: "lab", Contexts: map[string]contextEntry{"lab": {URL: base, Origin: base}}}
	if err := saveConfig(configFile, cfg); err != nil {
		t.Fatal(err)
	}
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	invoke := func(input string, args ...string) (int, string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		a := append([]string{"--config", configFile, "--output", "json"}, args...)
		code := run(a, strings.NewReader(input), &out, &stderr, transport)
		return code, out.String(), stderr.String()
	}
	if code, _, msg := invoke("fake-session\n", "login", "--token-stdin"); code != 0 {
		t.Fatalf("login: %d %s", code, msg)
	}
	info, err := os.Stat(sessionPath(configFile, "lab", cfg.Contexts["lab"]))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("session permissions: %v %v", info, err)
	}
	input := filepath.Join(dir, "request.json")
	if err := os.WriteFile(input, []byte(`{"swarm":"s1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{{"whoami"}, {"clusters", "list"}, {"clusters", "get", "c1"}, {"clusters", "create", "--file", input}, {"clusters", "plan", "c1", "--file", input}, {"clusters", "delete", "c1"}, {"clusters", "approve", "op1", "abc"}, {"operations", "list"}, {"operations", "watch", "op1"}, {"operations", "abort", "op1"}, {"backups", "list"}, {"backups", "run", "c1"}, {"backups", "verify", "c1"}, {"hosts", "list"}, {"services", "plan", "--file", input}, {"services", "create", "--file", input}, {"vuln", "findings", "list"}, {"vuln", "approve", "p1", "abc"}, {"plan", "export", "--file", input, "--format", "yaml"}}
	for _, cmd := range commands {
		code, out, msg := invoke("", cmd...)
		if code != 0 || out == "" {
			t.Errorf("%v: code %d out %q err %q", cmd, code, out, msg)
		}
	}
	resultFile := filepath.Join(dir, "inspection.json")
	if code, out, msg := invoke("", "backups", "verify", "c1", "--result-file", resultFile); code != 0 || strings.Contains(out, "private-value") {
		t.Fatalf("inspection: %d %s %s", code, out, msg)
	}
	result, err := os.ReadFile(resultFile)
	if err != nil || !strings.Contains(string(result), "private-value") {
		t.Fatalf("inspection result: %s %v", result, err)
	}
	if len(calls) < len(commands) {
		t.Fatalf("only %d API calls", len(calls))
	}
	if code, _, msg := invoke("", "logout"); code != 0 {
		t.Fatalf("logout: %d %s", code, msg)
	}
	if code, _, _ := invoke("", "clusters", "list"); code != 3 {
		t.Fatalf("after logout exit %d", code)
	}
}

func TestCLIErrorsAndTokenSources(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/session" {
			w.Write([]byte(`{"accountId":"a"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		w.Write([]byte(`{"error":"busy"}`))
	}))
	defer s.Close()
	dir := t.TempDir()
	cf := filepath.Join(dir, "cli.json")
	if err := saveConfig(cf, config{Current: "x", Contexts: map[string]contextEntry{"x": {URL: s.URL}}}); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if n := run([]string{"--config", cf, "login", "--token-file", tokenFile}, nil, &out, &stderr, s.Client().Transport); n != 0 {
		t.Fatalf("login file: %d %s", n, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if n := run([]string{"--config", cf, "clusters", "list"}, nil, &out, &stderr, s.Client().Transport); n != 5 || !strings.Contains(stderr.String(), "busy") {
		t.Fatalf("conflict: %d %s", n, stderr.String())
	}
	if _, err := id("bad/id"); err == nil {
		t.Fatal("slash accepted")
	}
	if _, err := tokenFrom(strings.NewReader("bad;cookie"), ""); err == nil {
		t.Fatal("cookie injection accepted")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCLIWithoutListener(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "cli.json")
	if err := saveConfig(cf, config{Current: "x", Contexts: map[string]contextEntry{"x": {URL: "https://manager.example", Origin: "https://manager.example"}}}); err != nil {
		t.Fatal(err)
	}
	tr := transportFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		switch r.URL.Path {
		case "/api/v1/auth/session":
			rec.WriteString(`{"accountId":"admin"}`)
		case "/api/v1/backups":
			rec.WriteString(`{"items":[{"cluster":"c1"}]}`)
		case "/api/v1/clusters":
			rec.WriteString(`{"items":[{"id":"c1"}]}`)
		default:
			rec.WriteHeader(404)
			rec.WriteString(`{"error":"not_found"}`)
		}
		return rec.Result(), nil
	})
	invoke := func(in string, a ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		n := run(append([]string{"--config", cf}, a...), strings.NewReader(in), &out, &err, tr)
		return n, out.String() + err.String()
	}
	if n, msg := invoke("test-token\n", "login", "--token-stdin"); n != 0 {
		t.Fatalf("login %d %s", n, msg)
	}
	if n, msg := invoke("", "clusters", "list"); n != 0 || !strings.Contains(msg, "c1") {
		t.Fatalf("list %d %s", n, msg)
	}
	if n, msg := invoke("", "backups", "verify", "c1"); n != 7 || !strings.Contains(msg, "no passed") {
		t.Fatalf("verify %d %s", n, msg)
	}
	if n, msg := invoke("", "clusters", "get", "missing"); n != 4 || !strings.Contains(msg, "not_found") {
		t.Fatalf("missing %d %s", n, msg)
	}
}

func TestCLIWithHTTPTestServer(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("__Host-dcm_session"); err != nil {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/api/v1/auth/session" {
			w.Write([]byte(`{"accountId":"admin"}`))
			return
		}
		if r.URL.Path == "/api/v1/clusters" {
			w.Write([]byte(`{"items":[{"id":"c1"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer s.Close()
	cf := filepath.Join(t.TempDir(), "cli.json")
	if err := saveConfig(cf, config{Current: "lab", Contexts: map[string]contextEntry{"lab": {URL: s.URL, Origin: s.URL}}}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if n := run([]string{"--config", cf, "login", "--token-stdin"}, strings.NewReader("session\n"), &out, &stderr, s.Client().Transport); n != 0 {
		t.Fatalf("login: %d %s", n, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if n := run([]string{"--config", cf, "clusters", "list"}, nil, &out, &stderr, s.Client().Transport); n != 0 || !strings.Contains(out.String(), "c1") {
		t.Fatalf("list: %d %s %s", n, out.String(), stderr.String())
	}
}
