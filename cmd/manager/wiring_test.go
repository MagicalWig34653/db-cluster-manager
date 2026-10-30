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

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// ---- image composer ---------------------------------------------------

func TestImageComposerIsOffByDefault(t *testing.T) {
	for name, sec := range map[string]map[string]any{
		"absent":   {"imageBuild": map[string]any{"workDir": t.TempDir()}},
		"disabled": {"imageBuild": map[string]any{"workDir": t.TempDir()}, "imageComposer": map[string]any{"enabled": false}},
	} {
		t.Run(name, func(t *testing.T) {
			rt, err := wiringRuntime(t, sec, false)
			if err != nil {
				t.Fatal(err)
			}
			admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
			if rec := call(t, rt, admin, "GET", "/image-composer/catalog", ""); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("catalog = %d, want 503 while off", rec.Code)
			}
		})
	}
}

func TestImageComposerEnabledServesCatalogPreviewAndRejectsBadBuilds(t *testing.T) {
	rt, err := wiringRuntime(t, map[string]any{
		"imageBuild":    map[string]any{"workDir": t.TempDir()},
		"imageComposer": map[string]any{"enabled": true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
	viewer := sessionFor(t, rt, "viewer", authz.RoleViewer)
	rec := call(t, rt, admin, "GET", "/image-composer/catalog", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "components") {
		t.Fatalf("catalog = %d %.200s", rec.Code, rec.Body.String())
	}
	// The recipe is validated server side; an empty one answers findings, not an error.
	rec = call(t, rt, admin, "POST", "/image-composer/preview", `{"recipe":{}}`)
	var pv api.ComposerPreview
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pv) != nil || pv.Valid || len(pv.Issues) == 0 {
		t.Fatalf("preview = %d %.300s", rec.Code, rec.Body.String())
	}
	if rec := call(t, rt, admin, "POST", "/image-composer/builds", `{"recipe":{},"target":{"registry":"r","repository":"a/b","tag":"1"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid recipe build = %d, want 400", rec.Code)
	}
	if rec := call(t, rt, viewer, "POST", "/image-composer/preview", `{"recipe":{}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer preview = %d, want 403", rec.Code)
	}
}

func TestImageComposerConfigValidation(t *testing.T) {
	if _, err := wiringRuntime(t, map[string]any{"imageComposer": map[string]any{"enabled": true}}, false); err == nil || !strings.Contains(err.Error(), "imageBuild") {
		t.Fatalf("composer without imageBuild: %v", err)
	}
	cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"dataDir": t.TempDir(), "bind": "127.0.0.1:0",
		"imageBuild": map[string]any{"workDir": t.TempDir()}, "imageComposer": map[string]any{"enabled": true}}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "auth.enabled") {
		t.Fatalf("composer without auth: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"imageComposer": map[string]any{"enable": true}}})); err == nil {
		t.Fatal("unknown imageComposer field accepted")
	}
}

// ---- fresh-server bootstrap -------------------------------------------

type fixedProbe struct{}

func (fixedProbe) Probe(context.Context, provision.Host) (provision.HostKey, error) {
	return provision.HostKey{Type: "ssh-ed25519", Fingerprint: "SHA256:fixedfingerprintfixedfingerprintfixedfing", KnownHosts: "10.0.0.5 ssh-ed25519 AAAA\n"}, nil
}

// noSSH fails every command: nothing in these tests may reach a host.
type noSSH struct{ calls int }

func (n *noSSH) Run(context.Context, provision.Host, provision.Credential, string, string) (string, error) {
	n.calls++
	return "", os.ErrPermission
}

func stubSSH(t *testing.T) *noSSH {
	t.Helper()
	runner, probe := newSSHRunner, newHostKeyProbe
	n := &noSSH{}
	newSSHRunner = func() provision.CommandRunner { return n }
	newHostKeyProbe = func() provision.HostKeyProbe { return fixedProbe{} }
	t.Cleanup(func() { newSSHRunner, newHostKeyProbe = runner, probe })
	return n
}

func bootstrapSection() map[string]any {
	return map[string]any{"enabled": true, "agentRelease": map[string]any{
		"version": "0.1.0", "binaryUrl": "https://releases.example.test/dbcm-agent-linux-amd64", "sha256": strings.Repeat("ab", 32)}}
}

func TestBootstrapIsOffByDefault(t *testing.T) {
	for name, sec := range map[string]any{"absent": nil, "disabled": map[string]any{"enabled": false}} {
		t.Run(name, func(t *testing.T) {
			m := map[string]any{}
			if sec != nil {
				m["serverBootstrap"] = sec
			}
			rt, err := wiringRuntime(t, m, true)
			if err != nil {
				t.Fatal(err)
			}
			admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
			if rec := call(t, rt, admin, "GET", "/bootstrap/drafts", ""); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET /bootstrap = %d, want 503 while off", rec.Code)
			}
		})
	}
}

func TestBootstrapConfigValidation(t *testing.T) {
	good := bootstrapSection()
	with := func(mut func(map[string]any)) map[string]any {
		b := map[string]any{}
		raw, _ := json.Marshal(good)
		_ = json.Unmarshal(raw, &b)
		mut(b)
		return b
	}
	for name, tc := range map[string]struct {
		sec       map[string]any
		transport bool
		want      string
	}{
		"needs transport":  {good, false, "agentTransport"},
		"needs release":    {map[string]any{"enabled": true}, true, "agentRelease"},
		"http binary":      {with(func(b map[string]any) { b["agentRelease"].(map[string]any)["binaryUrl"] = "http://x.test/a" }), true, "binaryUrl"},
		"short checksum":   {with(func(b map[string]any) { b["agentRelease"].(map[string]any)["sha256"] = "abc" }), true, "sha256"},
		"no version":       {with(func(b map[string]any) { b["agentRelease"].(map[string]any)["version"] = "" }), true, "version"},
		"sweep too fast":   {with(func(b map[string]any) { b["sweepSeconds"] = 5 }), true, "sweepSeconds"},
		"unknown disabled": {map[string]any{"enabled": false}, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := wiringRuntime(t, map[string]any{"serverBootstrap": tc.sec}, tc.transport)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
}

// TestBootstrapDraftsAreSealedAndSurviveRestart: the draft and the root
// password are written to the replicated store (sealed), the manager is
// restarted on the same data directory, and the new process still holds the
// draft and the credentials. Cancel deletes the credentials.
func TestBootstrapDraftsAreSealedAndSurviveRestart(t *testing.T) {
	stubSSH(t)
	dir := t.TempDir()
	bind := freeLoopback(t)
	enrollAddr := freeLoopback(t)
	cfgPath := writeConfig(t, map[string]any{
		"auth": map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": map[string]any{"dataDir": dir, "bind": bind, "serverBootstrap": bootstrapSection(),
			"agentTransport": map[string]any{"enrollListen": enrollAddr, "managerUrl": "https://" + enrollAddr}},
	})
	start := func() *managedRuntime {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
		waitUntil(t, "bootstrap service", func() bool { return rt.bootstrap.Load() != nil })
		return rt
	}
	rt := start()
	admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
	if rec := call(t, rt, admin, "GET", "/bootstrap/support", ""); rec.Code != http.StatusOK {
		t.Fatalf("support = %d", rec.Code)
	}
	const pw = "root-password-never-in-clear-77"
	body := `{"id":"boot1","host":{"id":"fresh1","address":"10.0.0.5","user":"root"},
	  "profile":{"serviceUser":"dbcm","runtime":"none","cluster":{"platform":"none"}},
	  "credential":{"password":"` + pw + `"},"joinToken":"SWMTKN-1-join-never-in-clear"}`
	rec := call(t, rt, admin, "POST", "/bootstrap/drafts", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("draft = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), pw) || strings.Contains(rec.Body.String(), "SWMTKN") {
		t.Fatal("credential echoed")
	}
	for _, c := range []string{manager.CollBootstrapDrafts, manager.CollBootstrapSecrets} {
		raw, ok, _ := rt.svc.Store().Raw(c, "boot1")
		if !ok || bytes.Contains(raw.Value, []byte(pw)) || bytes.Contains(raw.Value, []byte("SWMTKN")) {
			t.Fatalf("%s: stored=%v or in clear", c, ok)
		}
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}

	rt = start()
	t.Cleanup(func() { _ = rt.Close() })
	admin = sessionFor(t, rt, "admin2", authz.RoleAdministrator)
	rec = call(t, rt, admin, "GET", "/bootstrap/drafts/boot1", "")
	var v provision.BootstrapView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.Status != provision.StatusAwaitingFingerprint || !v.CredentialsHeld || v.HostKey == nil {
		t.Fatalf("after restart: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, rt, admin, "POST", "/bootstrap/drafts/boot1/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := rt.svc.Store().Raw(manager.CollBootstrapSecrets, "boot1"); ok {
		t.Fatal("credentials survived the cancel")
	}
	// Roles: only the administrator reaches the bootstrap.
	op := sessionFor(t, rt, "op", authz.RoleOperator)
	if rec := call(t, rt, op, "GET", "/bootstrap/drafts", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator = %d", rec.Code)
	}
}

func TestBootstrapServiceWiring(t *testing.T) {
	stubSSH(t)
	rt, err := wiringRuntime(t, map[string]any{"serverBootstrap": bootstrapSection()}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "bootstrap service", func() bool { return rt.bootstrap.Load() != nil })
	svc := rt.bootstrap.Load()
	if svc.Store == nil || svc.Term == nil || svc.EnvFunc == nil {
		t.Fatalf("incomplete wiring: %+v", svc)
	}
	if svc.Term() == 0 {
		t.Fatal("the leader has no term")
	}
	// Without installation settings the plan environment is not available.
	if _, err := svc.EnvFunc(); err == nil {
		t.Fatal("environment without installation settings")
	}
	if _, err := manager.Put(rt.svc.Store(), manager.CollSettings, manager.SettingsKey, manager.Settings{InstallationID: "inst-1"}, nil); err != nil {
		t.Fatal(err)
	}
	env, err := svc.EnvFunc()
	if err != nil || env.Installation != "inst-1" || env.CAFingerprint == "" || !strings.HasPrefix(env.ManagerURL, "https://") || env.Release.Version != "0.1.0" {
		t.Fatalf("%+v %v", env, err)
	}
}

// ---- patch SSH install -------------------------------------------------

func TestPatchInstallRoutesUseTheSealedDraftStore(t *testing.T) {
	stubSSH(t)
	newSSHRunner = func() provision.CommandRunner { return preflightSSH{} }
	rt, err := wiringRuntime(t, map[string]any{"patch": map[string]any{"enabled": true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "patch service", func() bool { return rt.patch.Load() != nil })
	admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
	const key = "ssh-private-key-never-in-clear-91"
	body := `{"request":{"id":"inst1","host":{"id":"h1","address":"h1.example.test","user":"admin","role":"worker"},"mode":"report",
	  "binaryUrl":"https://releases.example.test/agent","binarySha256":"` + strings.Repeat("ab", 32) + `","version":"0.9.0",
	  "managerUrl":"https://manager.example.test:7444","caFingerprint":"` + strings.Repeat("AB", 32) + `","installation":"inst-1"},
	  "credential":{"privateKey":"` + key + `","knownHosts":"h1.example.test ssh-ed25519 AAAA"}}`
	rec := call(t, rt, admin, "POST", "/patch/install/plans", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install plan = %d %s (503 means the installer is not wired)", rec.Code, rec.Body.String())
	}
	var plan patch.InstallPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	raw, ok, _ := rt.svc.Store().Raw(patch.CollInstall, "inst1")
	if !ok || bytes.Contains(raw.Value, []byte(key)) {
		t.Fatalf("draft stored=%v or the credential is in clear", ok)
	}
	// The host has no enrolled host record, so no enrollment token can be
	// issued: the execute fails before the host is touched and the draft stays.
	rec = call(t, rt, admin, "POST", "/patch/install/plans/inst1/execute", `{"digest":"`+plan.Digest+`"}`)
	if rec.Code == http.StatusAccepted {
		t.Fatalf("install executed without an enrolled host: %s", rec.Body.String())
	}
	if _, ok, _ := rt.svc.Store().Raw(patch.CollInstall, "inst1"); !ok {
		t.Fatal("draft lost although the host was never touched")
	}
}

type preflightSSH struct{}

func (preflightSSH) Run(_ context.Context, _ provision.Host, _ provision.Credential, cmd, _ string) (string, error) {
	if strings.Contains(cmd, "/etc/os-release") {
		return "os=debian\narch=aarch64\nsystemd=yes\npriv=sudo\nmanager=apt-get\nagent=absent\ndisk=5000000000\n", nil
	}
	return "", nil
}

// ---- backup copies -----------------------------------------------------

func TestBackupCopyAndMetastoreBackupRoutesAreReachable(t *testing.T) {
	backend := filepath.Join(t.TempDir(), "backend.json")
	target := filepath.Join(t.TempDir(), "dr.json")
	for _, f := range []string{backend, target} {
		if err := os.WriteFile(f, []byte(`{"type":"dir","dir":"`+t.TempDir()+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := wiringRuntime(t, map[string]any{"backup": map[string]any{
		"backendConfigFile": backend, "targets": map[string]any{"dr": target}, "metastoreTarget": "dr"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	b := rt.backups.Load()
	if b == nil || b.Targets["dr"] == nil || b.MetaTarget != "dr" || b.Backend == nil || b.Keys == nil {
		t.Fatalf("backups not wired with the configured targets: %+v", b)
	}
	// The command service implements the optional route interfaces. The
	// routes answer 503 when this assertion fails.
	var _ api.BackupTransfers = b
	var _ api.MetastoreBackups = b
	admin := sessionFor(t, rt, "admin", authz.RoleAdministrator)
	rec := call(t, rt, admin, "GET", "/backup-copies", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items"`) {
		t.Fatalf("GET /backup-copies = %d %s", rec.Code, rec.Body.String())
	}
	// A copy of an unknown cluster is a 404, not a 503.
	if rec := call(t, rt, admin, "POST", "/clusters/nope/backup-copies", `{"backup":"b1","target":"dr"}`); rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("copy start = 503: %s", rec.Body.String())
	}
	if rec := call(t, rt, admin, "POST", "/backup-copies/cp-x/restore-proposal", `{}`); rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("restore proposal = 503: %s", rec.Body.String())
	}
	// The metastore backup routes are mounted too. Creating one needs the
	// backup recipient key in the settings, so an empty installation answers
	// 409 settings_incomplete (not 503); listing works.
	if rec := call(t, rt, admin, "GET", "/settings/metastore-backups", ""); rec.Code != http.StatusOK {
		t.Fatalf("list metastore backups = %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, rt, admin, "POST", "/settings/metastore-backups", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "settings_incomplete") {
		t.Fatalf("create metastore backup = %d %s", rec.Code, rec.Body.String())
	}
}
