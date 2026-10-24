// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// fakeDocker records the docker command, echoes the registry password the
// way a misbehaving tool might, and writes the build metadata.
type fakeDocker struct {
	mu   sync.Mutex
	cmds []imagebuild.Command
	pw   string
}

func (f *fakeDocker) Run(_ context.Context, c imagebuild.Command) error {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	fmt.Fprintln(c.Stdout, "pushing with password "+f.pw)
	for i, a := range c.Args {
		if a == "--metadata-file" {
			return os.WriteFile(c.Args[i+1], []byte(`{"containerimage.digest":"sha256:`+strings.Repeat("c", 64)+`"}`), 0o600)
		}
	}
	return fmt.Errorf("no metadata file argument")
}

func startImageRuntime(t *testing.T, section map[string]any, runner imagebuild.CommandRunner) *managedRuntime {
	t.Helper()
	old := newImageRunner
	newImageRunner = func() imagebuild.CommandRunner { return runner }
	t.Cleanup(func() { newImageRunner = old })
	mgr := map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t)}
	if section != nil {
		mgr["imageBuild"] = section
	}
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": mgr,
	}))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	waitUntil(t, "leader API", func() bool { return rt.images.Load() != nil })
	return rt
}

// TestManagedModeWiresImageBuilds runs a build through the manager's real
// stores with a fake docker: the routes are mounted, the registry password
// is sealed and never reaches the job, and the authorizer decides.
func TestManagedModeWiresImageBuilds(t *testing.T) {
	const pw = "pw-wired-secret-4321"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer reg.Close()
	fd := &fakeDocker{pw: pw}
	rt := startImageRuntime(t, map[string]any{"workDir": t.TempDir()}, fd)
	be := rt.images.Load()
	if be.Builds == nil || be.Regs == nil {
		t.Fatalf("backend not complete: %+v", be)
	}
	mux := newMux(rt)
	for _, p := range []string{"/api/v1/image-builds", "/api/v1/registries"} {
		if rec := serve(t, mux, "GET", p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401 (mounted, session required)", p, rec.Code)
		}
	}
	ctx := context.Background()
	admin := authz.Subject{ID: "admin", Kind: authz.SubjectUser}
	view, err := be.AddExternalRegistry(ctx, admin, api.ExternalRegistryInput{ID: "main", Name: "Main", Kind: "generic", Endpoint: reg.URL, Username: "bot", Password: pw})
	if err != nil || !view.CredentialSet {
		t.Fatalf("register: %+v %v", view, err)
	}
	req := imagebuild.Request{
		Spec: imagebuild.WebServerSpec{Name: "shop",
			Nginx: imagebuild.NginxSpec{Version: "1.27", BaseDigest: "sha256:" + strings.Repeat("a", 64)}},
		Target: imagebuild.Target{Registry: "main", Repository: "team/shop", Tag: "1"},
	}
	plan, err := be.Plan(ctx, admin, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ApprovalDigest = "sha256:wrong"
	if _, err := be.Start(ctx, admin, req); err != api.ErrApprovalMismatch {
		t.Fatalf("wrong digest: %v", err)
	}
	req.ApprovalDigest = plan.Digest
	job, err := be.Start(ctx, admin, req)
	if err != nil {
		t.Fatal(err)
	}
	// Builds run in the background in the manager.
	var done imagebuild.Job
	waitUntil(t, "job finished", func() bool {
		done, err = be.Builds.Get(ctx, job.ID)
		return err == nil && done.Status != imagebuild.JobRunning
	})
	if done.Status != imagebuild.JobSucceeded || done.ImageDigest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("job %+v", done)
	}
	if strings.Contains(done.Log, pw) || !strings.Contains(done.Log, "REDACTED") {
		t.Fatalf("job log not redacted: %q", done.Log)
	}
	if len(fd.cmds) != 1 || fd.cmds[0].Name != "docker" || fd.cmds[0].Args[0] != "buildx" {
		t.Fatalf("commands %+v", fd.cmds)
	}
}

// TestImageBuildDisabledWithoutSection: registries work, builds stay
// unwired (the routes answer 503 for an authorized caller).
func TestImageBuildDisabledWithoutSection(t *testing.T) {
	rt := startImageRuntime(t, nil, &fakeDocker{})
	be := rt.images.Load()
	if be.Regs == nil || be.Builds != nil {
		t.Fatalf("backend %+v", be)
	}
}

func TestImageBuildConfigValidation(t *testing.T) {
	for name, sec := range map[string]map[string]any{
		"relative workDir": {"workDir": "tmp/builds"},
		"tiny log cap":     {"maxLogBytes": 10},
		"binary with args": {"dockerBinary": "docker --debug"},
	} {
		cfg, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{
			"dataDir": t.TempDir(), "bind": "127.0.0.1:0", "imageBuild": sec}}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := loadConfig(writeConfig(t, map[string]any{"manager": map[string]any{"imageBuild": map[string]any{"dockerBin": "x"}}})); err == nil {
		t.Error("unknown imageBuild field accepted")
	}
}

func TestInitImageBuildFlag(t *testing.T) {
	out := filepath.Join(t.TempDir(), "c.json")
	var stderr bytes.Buffer
	args := append(append([]string{}, exampleArgs...), "-image-build", "-out", out)
	if err := runInit(args, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	ib := cfg.Manager.ImageBuild
	if ib == nil || ib.DockerBinary != "docker" || ib.MaxLogBytes != imagebuild.DefaultMaxLogBytes {
		t.Fatalf("imageBuild %+v", ib)
	}
	if !strings.Contains(stderr.String(), "no docker CLI") {
		t.Error("instructions do not warn about the missing docker CLI")
	}
}
