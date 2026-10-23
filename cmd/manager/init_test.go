// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
)

// shippedExample is deploy/manager.example.json; exampleArgs regenerate it
// (docs/INSTALL.md quotes the same command).
const shippedExample = "../../deploy/manager.example.json"

var exampleArgs = []string{"-rp-id", "dbcm.example.com", "-manager-addr", "192.0.2.10", "-installation", "example"}

// TestShippedExampleConfig loads the shipped example with the real config
// loader, applies the start-time validation and checks that `manager init`
// still produces it byte for byte.
func TestShippedExampleConfig(t *testing.T) {
	cfg, err := loadConfig(shippedExample)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfigStatic(cfg); err != nil {
		t.Fatal(err)
	}
	mc := cfg.Manager
	if mc == nil || !cfg.Auth.Enabled || mc.DataDir != containerDataDir || mc.AgentTransport == nil || mc.Settings == nil {
		t.Fatalf("example is not a managed-mode config: %+v", cfg)
	}
	if mc.Bind != "127.0.0.1:7445" || len(mc.Peers) != 0 || mc.LabAgents {
		t.Fatalf("example must be single-instance on loopback without lab agents: %+v", mc)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "gen.json")
	var stderr bytes.Buffer
	if err := runInit(append(append([]string{}, exampleArgs...), "-out", out), &bytes.Buffer{}, &stderr); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(shippedExample)
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, want) {
		t.Fatalf("deploy/manager.example.json is stale; regenerate with: manager init %s -out deploy/manager.example.json\n%s", strings.Join(exampleArgs, " "), got)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config written with mode %v", fi.Mode().Perm())
	}
	for _, s := range []string{"setup token", "docker stack deploy -c deploy/stack.yaml", "dbcm-example-monitor-pw", "DBCM_DOCKER_GID"} {
		if !strings.Contains(stderr.String(), s) {
			t.Errorf("instructions lack %q", s)
		}
	}
}

// TestExampleConfigStartsManagedRuntime starts the managed runtime with the
// shipped example, changing only what a test host cannot provide (data
// directory and listen addresses), and checks that the leader serves the
// API and applies the configured settings.
func TestExampleConfigStartsManagedRuntime(t *testing.T) {
	cfg, err := loadConfig(shippedExample)
	if err != nil {
		t.Fatal(err)
	}
	enroll := freeLoopback(t)
	mc := cfg.Manager
	mc.DataDir, mc.Bind = t.TempDir(), freeLoopback(t)
	mc.AgentTransport.EnrollListen, mc.AgentTransport.ManagerURL = enroll, "https://"+enroll
	var out bytes.Buffer
	rt, err := newManagedRuntime(cfg, quietLog(), &out)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	got, ok, err := manager.Get[manager.Settings](rt.svc.Store(), manager.CollSettings, manager.SettingsKey)
	if err != nil || !ok {
		t.Fatalf("settings %v %v", ok, err)
	}
	if s := got.Value; s.InstallationID != "example" || s.AgentImage != "db-cluster-manager:local" || s.Accounts.BackupSecret != "dbcm-example-backup-pw" {
		t.Fatalf("settings %+v", s)
	}
	if !strings.Contains(out.String(), "setup token") {
		t.Fatal("no setup token on a fresh installation")
	}
}

func TestInitHAWithBackup(t *testing.T) {
	dir := t.TempDir()
	keyFile, cfgFile := filepath.Join(dir, "recovery.key"), filepath.Join(dir, "m2.json")
	var stdout, stderr bytes.Buffer
	err := runInit([]string{"-rp-id", "dbcm.example.com", "-manager-addr", "mgr2.example.com", "-installation", "prod-1",
		"-node-id", "manager-2", "-peers", "manager-1=192.0.2.10:7445,manager-2=192.0.2.11:7445,manager-3=192.0.2.12:7445",
		"-backup", "-recovery-key-out", keyFile, "-job-image", "jobs@sha256:" + strings.Repeat("a", 64), "-out", cfgFile}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Manager
	if !mc.Keyring || mc.PeerTLS == nil || mc.PeerTLS.KeyFile != containerPeerKey || mc.Bind != "192.0.2.11:7445" || mc.Advertise != mc.Bind || len(mc.Peers) != 3 {
		t.Fatalf("HA section %+v", mc)
	}
	if mc.AgentTransport.ManagerURL != "https://mgr2.example.com:7444" || len(mc.AgentTransport.ServerDNS) != 1 {
		t.Fatalf("agent transport %+v", mc.AgentTransport)
	}
	if mc.Backup == nil || mc.Backup.BackendConfigFile != containerBackendFile || mc.Settings.Backup.BackendSecret != "dbcm-prod-1-backend" {
		t.Fatalf("backup %+v %+v", mc.Backup, mc.Settings.Backup)
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("recovery key mode %v", fi.Mode().Perm())
	}
	k, err := backup.ParseRecoveryKey(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.StreamPublicKey()
	if mc.Settings.Backup.RecipientKey != pub.String() {
		t.Fatal("recipient key is not the public half of the written recovery key")
	}
	cfgRaw, _ := os.ReadFile(cfgFile)
	secret := strings.TrimSpace(string(raw))
	for name, s := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String(), "config": string(cfgRaw)} {
		if strings.Contains(s, secret) {
			t.Fatalf("recovery key leaked to %s", name)
		}
	}
	if !strings.Contains(stderr.String(), "Three-replica variant") {
		t.Fatal("HA instructions missing")
	}
	// Neither file is overwritten.
	if err := runInit([]string{"-rp-id", "x.example.com", "-manager-addr", "192.0.2.1", "-out", cfgFile}, &stdout, &stderr); err == nil {
		t.Fatal("existing config overwritten")
	}
	if err := runInit([]string{"-rp-id", "x.example.com", "-manager-addr", "192.0.2.1", "-backup", "-recovery-key-out", keyFile}, &stdout, &stderr); err == nil {
		t.Fatal("existing recovery key overwritten")
	}
}

func TestInitRejectsInvalidInput(t *testing.T) {
	base := []string{"-rp-id", "dbcm.example.com", "-manager-addr", "192.0.2.10"}
	for name, args := range map[string][]string{
		"no rp id":            {"-manager-addr", "192.0.2.10"},
		"no manager addr":     {"-rp-id", "dbcm.example.com"},
		"addr with port":      append(base[:2:2], "-manager-addr", "192.0.2.10:7444"),
		"bad installation":    append(base, "-installation", "Prod_1"),
		"http origin":         append(base, "-origin", "http://dbcm.example.com"),
		"relative volumes":    append(base, "-volume-base", "volumes"),
		"relative data dir":   append(base, "-data-dir", "data"),
		"even peers":          append(base, "-peers", "manager-1=a:1,manager-2=b:1"),
		"self not in peers":   append(base, "-node-id", "manager-9", "-peers", "manager-1=a:1,manager-2=b:1,manager-3=c:1"),
		"duplicate peer":      append(base, "-peers", "manager-1=a:1,manager-1=b:1,manager-3=c:1"),
		"backup without key":  append(base, "-backup"),
		"key without backup":  append(base, "-recipient-key", "dbcmpk1:x"),
		"bad recipient key":   append(base, "-backup", "-recipient-key", "dbcmpk1:x"),
		"positional argument": append(base, "extra"),
	} {
		if err := runInit(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// `manager init` is reachable through the normal entry point.
	out := filepath.Join(t.TempDir(), "c.json")
	if err := run(append([]string{"init"}, append(base, "-out", out)...), func(string) string { return "" }, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(out); err != nil {
		t.Fatal(err)
	}
}
