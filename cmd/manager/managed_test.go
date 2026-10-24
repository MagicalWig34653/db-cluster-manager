// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
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

// TestManagerPeersValidation: lab E4 re-run B configured node IDs such as
// "manager-192.168.104.11" next to peers manager-1..3 (a shell quoting
// error); the replicas started and none ever led. Such a config is refused.
func TestManagerPeersValidation(t *testing.T) {
	peers := []peerConfig{{ID: "manager-1", Address: "10.0.0.1:18490"}, {ID: "manager-2", Address: "10.0.0.2:18490"}}
	ok := managerConfig{DataDir: "/d", Bind: "10.0.0.1:18490", NodeID: "manager-1", Peers: peers}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(m *managerConfig){
		"node not a peer":  func(m *managerConfig) { m.NodeID = "manager-10.0.0.11" },
		"default not peer": func(m *managerConfig) { m.NodeID = ""; m.Peers = peers[1:]; m.Bind = "10.0.0.2:18490" },
		"own address":      func(m *managerConfig) { m.Advertise = "10.0.0.9:18490" },
		"duplicate id": func(m *managerConfig) {
			m.Peers = append(slices.Clone(peers), peerConfig{ID: "manager-2", Address: "10.0.0.3:1"})
		},
		"duplicate address": func(m *managerConfig) {
			m.Peers = append(slices.Clone(peers), peerConfig{ID: "manager-3", Address: "10.0.0.2:18490"})
		},
		"empty address": func(m *managerConfig) { m.Peers = append(slices.Clone(peers), peerConfig{ID: "manager-3"}) },
	} {
		m := ok
		m.Peers = slices.Clone(peers)
		mut(&m)
		if err := m.validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
}

func newProto() http.Handler { return api.NewHandler() }

func TestManagedModeAgentTransport(t *testing.T) {
	dir := t.TempDir()
	enrollAddr := freeLoopback(t)
	at := map[string]any{"enrollListen": enrollAddr, "managerUrl": "https://" + enrollAddr}
	cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"dataDir": dir, "bind": freeLoopback(t), "agentTransport": at}}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	waitUntil(t, "leadership", rt.svc.IsLeader)
	// The enrollment certificate needs the installation ID.
	if _, err := manager.Put(rt.svc.Store(), manager.CollSettings, manager.SettingsKey, manager.Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm"}, nil); err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}} //nolint:gosec // probing only
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	var resp *http.Response
	waitUntil(t, "enrollment endpoint", func() bool {
		r, err := c.Post("https://"+enrollAddr+"/v1/agent/enroll", "application/json", strings.NewReader("{}"))
		if err != nil {
			return false
		}
		resp = r
		return true
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty enrollment request = %d", resp.StatusCode)
	}
	if cs := resp.TLS; cs == nil || cs.Version != tls.VersionTLS13 || !certs.HasURI(cs.PeerCertificates[0], certs.ManagerURI("inst-1")) {
		t.Fatalf("enrollment TLS %+v", cs)
	}

	for name, m := range map[string]map[string]any{
		"lab and mtls": {"dataDir": t.TempDir(), "bind": "127.0.0.1:0", "labAgents": true, "agentTransport": at},
		"no listen":    {"dataDir": t.TempDir(), "bind": "127.0.0.1:0", "agentTransport": map[string]any{"managerUrl": "https://x:1"}},
		"http url":     {"dataDir": t.TempDir(), "bind": "127.0.0.1:0", "agentTransport": map[string]any{"enrollListen": "127.0.0.1:0", "managerUrl": "http://x:1"}},
		"bad ip":       {"dataDir": t.TempDir(), "bind": "127.0.0.1:0", "agentTransport": map[string]any{"enrollListen": "127.0.0.1:0", "managerUrl": "https://x:1", "serverIps": []string{"nope"}}},
	} {
		cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": m}))
		if err != nil {
			t.Fatal(err)
		}
		if rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil {
			_ = rt.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestManagedModeWiresCommandServices checks that the leader API mounts the
// command endpoints, that the monitoring service runs on the leader, and
// that the manager's system subject is bound (alerts, scheduled backups).
func TestManagedModeWiresCommandServices(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": map[string]any{"dataDir": dir, "bind": freeLoopback(t), "backup": map[string]any{"scheduleIntervalSeconds": 30}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mux := newMux(rt)
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	for _, p := range []string{"/api/v1/hosts", "/api/v1/backups", "/api/v1/settings/notifications", "/api/v1/clusters/c1/outage-diagnosis"} {
		if rec := serve(t, mux, "GET", p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401 (mounted, session required)", p, rec.Code)
		}
	}
	st := rt.stack.Load()
	b := st.Policy.Bindings(collect.SystemSubject)
	if len(b) != 1 || b[0].Role != authz.RoleManagerSystem || b[0].Scope != authz.Global {
		t.Fatalf("system bindings %+v", b)
	}
	if rt.backups.Load() == nil || rt.monitoring == nil {
		t.Fatal("command services not constructed")
	}
	waitUntil(t, "monitoring running", func() bool { return rt.monitoring.Status().Running })
	if _, err := os.Stat(filepath.Join(dir, "monitoring", "metrics")); err != nil {
		t.Fatalf("metric store: %v", err)
	}
	if d := rt.Check(collect.SystemSubject, authz.ActionRead, authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope("s", "c")}); !d.Allowed {
		t.Fatalf("system subject cannot read: %+v", d)
	}
}

func TestManagedModeMonitoringAndBackupConfig(t *testing.T) {
	good := filepath.Join(t.TempDir(), "backend.json")
	if err := os.WriteFile(good, []byte(`{"type":"dir","dir":"`+t.TempDir()+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"type":"s3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]map[string]any{
		"relative monitoring dir": {"monitoring": map[string]any{"dataDir": "metrics"}},
		"short schedule interval": {"backup": map[string]any{"scheduleIntervalSeconds": 5}},
		"relative backend file":   {"backup": map[string]any{"backendConfigFile": "backend.json"}},
		"missing backend file":    {"backup": map[string]any{"backendConfigFile": filepath.Join(t.TempDir(), "none.json")}},
		"invalid backend file":    {"backup": map[string]any{"backendConfigFile": bad}},
	} {
		m["dataDir"], m["bind"] = t.TempDir(), "127.0.0.1:0"
		cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": m}))
		if err != nil {
			t.Fatal(err)
		}
		if rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil {
			_ = rt.Close()
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"backup": map[string]any{"backendFile": "x"}}})); err == nil {
		t.Fatal("unknown backup field accepted")
	}
	cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t),
		"monitoring": map[string]any{"disabled": true}, "backup": map[string]any{"backendConfigFile": good}}}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.backend == nil {
		t.Fatal("backend not opened")
	}
	waitUntil(t, "leadership", rt.svc.IsLeader)
	time.Sleep(50 * time.Millisecond)
	if rt.monitoring.Status().Running {
		t.Fatal("disabled monitoring runs")
	}
}

// TestManagedModeAppliesConfiguredSettings: lab E4 found that nothing could
// set the installation settings (every command answered
// settings_incomplete) and that operations ran with zero retries; the
// "settings" section is merged by the leader and command services retry.
func TestManagedModeAppliesConfiguredSettings(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth": map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": map[string]any{"dataDir": dir, "bind": freeLoopback(t), "settings": map[string]any{
			"installationId": "e4", "volumeBase": "/var/lib/dbcm/volumes", "agentImage": "dbcm-agent:e4",
			"accounts": map[string]any{"monitorUser": "dbcm_monitor", "monitorSecret": "dbcm-e4-monitor"},
			"backup":   map[string]any{"jobImage": "dbcm-job:e4"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	got, ok, err := manager.Get[manager.Settings](rt.svc.Store(), manager.CollSettings, manager.SettingsKey)
	if err != nil || !ok {
		t.Fatalf("settings %v %v", ok, err)
	}
	s := got.Value
	if s.InstallationID != "e4" || s.VolumeBase != "/var/lib/dbcm/volumes" || s.AgentImage != "dbcm-agent:e4" ||
		s.Accounts.MonitorSecret != "dbcm-e4-monitor" || s.Backup.JobImage != "dbcm-job:e4" {
		t.Fatalf("settings %+v", s)
	}
	set, err := rt.commandServices(rt.stack.Load())
	if err != nil {
		t.Fatal(err)
	}
	if ops := set.control.Ops; ops.MaxRetries <= 0 {
		t.Fatalf("operations without retries: %+v", ops)
	}
}
