// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

func TestParseFlags(t *testing.T) {
	if o, err := parseFlags([]string{"-version"}); err != nil || !o.showVersion {
		t.Fatalf("-version: %+v %v", o, err)
	}
	if _, err := parseFlags(nil); err == nil || !strings.Contains(err.Error(), "-config") {
		t.Fatalf("missing -config: %v", err)
	}
	if _, err := parseFlags([]string{"-config", "x", "extra"}); err == nil {
		t.Fatal("extra argument accepted")
	}
	if o, err := parseFlags([]string{"-config", "/etc/p.json"}); err != nil || o.config != "/etc/p.json" {
		t.Fatalf("-config: %+v %v", o, err)
	}
}

func TestPrintVersion(t *testing.T) {
	var b bytes.Buffer
	if err := printVersion(&b); err != nil {
		t.Fatal(err)
	}
	var info version.Info
	if err := json.Unmarshal(b.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Component != "proxy" || info.Version != version.Version {
		t.Fatalf("info = %+v", info)
	}
}

func backend(t *testing.T, id string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, id+"\n")
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	return l.Addr().String()
}

func writeConfig(t *testing.T, path, a, b, preferred string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "write_listen": "127.0.0.1:0",
  "control_listen": "127.0.0.1:0",
  "nodes": [
    {"id": "a", "address": %q, "role": "writer-eligible"},
    {"id": "b", "address": %q, "role": "writer-eligible"}
  ],
  "policy": {"mode": "prefer-node", "preferred_id": %q, "switch_consecutive": 1, "stale_after": "1s", "drain_deadline": "100ms"},
  "probe": {"sync": "assume-synced", "interval": "50ms", "timeout": "50ms"},
  "limits": {"shutdown_grace": "500ms"}
}`, a, b, preferred)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func statusWriter(t *testing.T, addr string) string {
	t.Helper()
	r, err := http.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var s proxy.Status
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.Writer
}

func connectID(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return strings.TrimSpace(string(b))
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestRunReloadsOnSIGHUPAndStops(t *testing.T) {
	a, b := backend(t, "a"), backend(t, "b")
	path := filepath.Join(t.TempDir(), "proxy.json")
	writeConfig(t, path, a, b, "a")

	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal, 1)
	ready := make(chan *proxy.Proxy, 1)
	var logs safeBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, options{config: path}, &logs, hup, func(p *proxy.Proxy) { ready <- p }) }()
	var p *proxy.Proxy
	select {
	case p = <-ready:
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not start")
	}
	ctl, wr := p.ControlAddr(), p.Addr(proxy.EndpointWrite)
	waitUntil(t, "writer a", func() bool { return statusWriter(t, ctl) == "a" })
	if id := connectID(t, wr); id != "a" {
		t.Fatalf("connected to %q", id)
	}

	writeConfig(t, path, a, b, "b")
	hup <- syscall.SIGHUP
	waitUntil(t, "writer b after SIGHUP", func() bool { return statusWriter(t, ctl) == "b" })
	if id := connectID(t, wr); id != "b" {
		t.Fatalf("after reload connected to %q", id)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	hup <- syscall.SIGHUP
	waitUntil(t, "reload rejection logged", func() bool { return strings.Contains(logs.String(), "reload rejected") })
	if w := statusWriter(t, ctl); w != "b" {
		t.Fatalf("invalid reload changed writer to %s", w)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop")
	}
	if c, err := net.DialTimeout("tcp", wr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("still listening after stop")
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte(`{"write_listen": "127.0.0.1:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), options{config: path}, io.Discard, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("err = %v", err)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeGaleraConfig(t *testing.T, path, a, b, monitor string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "write_listen": "127.0.0.1:0",
  "control_listen": "127.0.0.1:0",
  "nodes": [
    {"id": "a", "address": %q, "role": "writer-eligible"},
    {"id": "b", "address": %q, "role": "writer-eligible"}
  ],
  "policy": {"mode": "prefer-node", "preferred_id": "a", "switch_consecutive": 1, "stale_after": "2s", "drain_deadline": "100ms"},
  "probe": {"sync": "galera", "interval": "300ms", "timeout": "300ms"%s},
  "limits": {"shutdown_grace": "500ms"}
}`, a, b, monitor)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunGaleraSyncRoutesOnlySyncedNode(t *testing.T) {
	// a is a Donor/Desynced node (preferred but unusable), b is Synced.
	a := fakemysql.New(t, "dbcm_monitor", "pw-1", fakemysql.WithVars(fakemysql.SyncedVars(),
		"wsrep_local_state", "2", "wsrep_local_state_comment", "Donor/Desynced"))
	b := fakemysql.New(t, "dbcm_monitor", "pw-1", fakemysql.SyncedVars())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "monitor.pw"), []byte("pw-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "proxy.json")
	writeGaleraConfig(t, path, a.Addr(), b.Addr(), `, "monitor": {"user": "dbcm_monitor", "password_file": "monitor.pw", "tls": "disabled"}`)

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan *proxy.Proxy, 1)
	hup := make(chan os.Signal, 1)
	var logs safeBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, options{config: path}, &logs, hup, func(p *proxy.Proxy) { ready <- p }) }()
	var p *proxy.Proxy
	select {
	case p = <-ready:
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not start")
	}
	waitUntil(t, "writer b", func() bool { return statusWriter(t, p.ControlAddr()) == "b" })

	// Rotate to a wrong password: nothing is usable any more (fail closed).
	if err := os.WriteFile(filepath.Join(dir, "monitor.pw"), []byte("wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "no writer after bad password", func() bool { return statusWriter(t, p.ControlAddr()) == "none" })
	if strings.Contains(logs.String(), "pw-1") || strings.Contains(logs.String(), "wrong") {
		t.Fatalf("password in logs: %s", logs.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunGaleraWithoutMonitorFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	writeGaleraConfig(t, path, "127.0.0.1:1", "127.0.0.1:2", "")
	err := run(context.Background(), options{config: path}, io.Discard, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "probe.monitor") {
		t.Fatalf("err = %v", err)
	}
}

func TestMonitorConfigValidation(t *testing.T) {
	for name, mon := range map[string]string{
		"inline password":   `, "monitor": {"user": "u", "password": "x", "tls": "disabled"}`,
		"no password":       `, "monitor": {"user": "u", "tls": "disabled"}`,
		"both sources":      `, "monitor": {"user": "u", "password_file": "f", "password_env": "E", "tls": "disabled"}`,
		"no tls choice":     `, "monitor": {"user": "u", "password_env": "E"}`,
		"ca with plaintext": `, "monitor": {"user": "u", "password_env": "E", "tls": "disabled", "ca_file": "ca.pem"}`,
		"no user":           `, "monitor": {"password_env": "E", "tls": "disabled"}`,
	} {
		path := filepath.Join(t.TempDir(), "proxy.json")
		writeGaleraConfig(t, path, "127.0.0.1:1", "127.0.0.1:2", mon)
		if _, err := proxy.LoadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
