// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// Every command kind the executor decodes must have an explicit entry in
// the routing table, and the table must not name kinds the executor does
// not know (the patch kinds excepted).
func TestRoutingTableCoversEveryAgentKind(t *testing.T) {
	var kinds []string
	for _, k := range AllKinds() {
		kinds = append(kinds, string(k))
		if _, ok := agentrt.RequirementFor(string(k)); !ok {
			t.Errorf("agent kind %q has no routing table entry", k)
		}
	}
	for _, k := range agentrt.Kinds() {
		if !strings.HasPrefix(k, agentrt.PatchKindPrefix) && !slices.Contains(kinds, k) {
			t.Errorf("routing table kind %q is not an agent kind", k)
		}
	}
}

func TestContainerAgentWithoutAllowListRefusesResticAndStampsRuntime(t *testing.T) {
	f := newFixture(t)
	if got := f.exec.RuntimeInfo(); got.Runtime != agentrt.Container || got.Has(agentrt.Restic) || !got.Has(agentrt.DockerObjects) {
		t.Fatalf("report %+v", got)
	}
	_, err := f.do(1, "op", "backup", KindResticBackup, ResticArgs{Backend: restic.BackendLocal, Repository: "/r", PasswordSecret: "pw", Paths: []string{"/srv"}})
	if !errors.Is(err, ErrInvalidCommand) || !strings.Contains(err.Error(), "restic") {
		t.Fatalf("restic without capability: %v", err)
	}
	res := f.mustDo(1, "op", "list", KindListOwned, ListOwnedArgs{ObjectKind: KindService})
	if res.Runtime != agentrt.Container {
		t.Errorf("result runtime %q", res.Runtime)
	}
	// The recorded result keeps the runtime on a replay.
	if again := f.mustDo(1, "op", "list", KindListOwned, ListOwnedArgs{ObjectKind: KindService}); !again.Replayed || again.Runtime != agentrt.Container {
		t.Errorf("replay %+v", again)
	}
}

func newNativeExecutor(t *testing.T, roots []string, runner restic.Runner, secrets string) *Executor {
	t.Helper()
	dir := t.TempDir()
	g, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(Config{
		InstallationID: testInstallation, StatePath: filepath.Join(dir, "native-state.json"), VolumeBase: dir, Guard: g,
		Info: agentrt.NativeInfo(len(roots) > 0), ResticAllowedPaths: roots, ResticRunner: runner,
		ResticProbeTimeout: -1, SecretsDir: secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestNativeAgentServesResticOnlyWithTheSameRules(t *testing.T) {
	root := t.TempDir()
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "restic-password"), []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeResticRunner{}
	e := newNativeExecutor(t, []string{root}, runner, secrets)
	if got := e.RuntimeInfo(); got.Runtime != agentrt.Native || !got.Has(agentrt.Restic) || got.Has(agentrt.DockerObjects) {
		t.Fatalf("report %+v", got)
	}
	do := func(step string, k Kind, args any) (Result, error) {
		return e.Handle(context.Background(), Command{Term: 1, OpID: "op", StepID: step, Kind: k, Args: mustArgs(t, args)})
	}
	a := ResticArgs{Backend: restic.BackendLocal, Repository: filepath.Join(root, "repo"), PasswordSecret: "restic-password", Paths: []string{root}}
	res, err := do("backup", KindResticBackup, a)
	if err != nil || res.Status != StatusSucceeded || res.Runtime != agentrt.Native {
		t.Fatalf("native restic: %+v %v", res, err)
	}
	// Same allow-list rule as the container agent: outside the roots, the
	// secrets directory and the Docker socket are refused.
	for _, p := range []string{"/etc", secrets, "/var/run/docker.sock"} {
		b := a
		b.Paths = []string{p}
		if res, err := do("bad-"+filepath.Base(p), KindResticBackup, b); err != nil || res.Status != StatusRefused || res.Code != CodeInvalidPath {
			t.Errorf("source %s: %+v %v", p, res, err)
		}
	}
	if len(runner.requests) != 1 {
		t.Fatalf("refused sources reached restic: %d runs", len(runner.requests))
	}
	// Docker-object and Galera steps are refused without effect and without
	// a step record, whatever the manager sent.
	for _, k := range []Kind{KindCreateService, KindRemoveVolume, KindExecSQL, KindCollectHealth, KindStopDB} {
		_, err := e.Handle(context.Background(), Command{Term: 1, OpID: "op", StepID: "x-" + string(k), Kind: k, Args: []byte(`{}`)})
		if !errors.Is(err, ErrInvalidCommand) || !strings.Contains(err.Error(), "native agent does not offer") {
			t.Errorf("%s on a native agent: %v", k, err)
		}
	}
}

func TestNativeAgentWithoutRoots(t *testing.T) {
	e := newNativeExecutor(t, nil, nil, t.TempDir())
	if e.RuntimeInfo().Has(agentrt.Restic) {
		t.Fatal("restic claimed without allowed paths")
	}
	_, err := e.Handle(context.Background(), Command{Term: 1, OpID: "op", StepID: "s", Kind: KindResticStats, Args: []byte(`{}`)})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("restic without roots: %v", err)
	}
}

func TestExecutorRejectsInconsistentReport(t *testing.T) {
	dir := t.TempDir()
	g, _ := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(dir, "term")))
	_, err := NewExecutor(Config{InstallationID: testInstallation, StatePath: filepath.Join(dir, "s.json"), VolumeBase: dir, Guard: g,
		Info: agentrt.Info{Runtime: agentrt.Container, Capabilities: []agentrt.Capability{agentrt.DockerObjects, agentrt.Patch}}, Docker: noDocker{}})
	if !errors.Is(err, agentrt.ErrMismatch) {
		t.Fatalf("container claiming patch: %v", err)
	}
	_, err = NewExecutor(Config{InstallationID: testInstallation, StatePath: filepath.Join(dir, "s.json"), VolumeBase: dir, Guard: g,
		Info: agentrt.NativeInfo(true)})
	if err == nil {
		t.Fatal("restic capability without allowed paths accepted")
	}
}

func TestHTTPResponsesCarryTheRuntimeReport(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(NewLabHTTPHandler(f.exec))
	defer srv.Close()
	// Loopback Host header and peer: the httptest server is on 127.0.0.1.
	var got agentrt.Info
	var present bool
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), OnInfo: func(i agentrt.Info, p bool, err error) {
		if err != nil {
			t.Errorf("report: %v", err)
		}
		got, present = i, p
	}}
	if _, err := c.Handle(context.Background(), Command{Term: 1, OpID: "op", StepID: "l", Kind: KindListOwned, Args: mustArgs(t, ListOwnedArgs{ObjectKind: KindService})}); err != nil {
		t.Fatal(err)
	}
	if !present || got.Runtime != agentrt.Container || !got.Has(agentrt.GaleraSteps) {
		t.Fatalf("report %+v present=%v", got, present)
	}
	// An error answer carries the report, too.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+LabCommandPath, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get(agentrt.HeaderRuntime) != "container" {
		t.Errorf("error answer headers %v", resp.Header)
	}
}
