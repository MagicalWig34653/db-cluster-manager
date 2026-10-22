// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// galeraSpec converts the Galera builder's spec of one node the way
// internal/orchestrate does for the fields start-db looks at (mounts by
// volume name).
func galeraSpec(t *testing.T, volume string) ServiceSpec {
	t.Helper()
	gs, err := galera.BuildServiceSpec(galera.Cluster{InstallationID: testInstallation, ClusterID: "cluster-1", Name: "shop",
		Image: "mariadb:11.4", Network: "net-1", RootPasswordSecret: "root-pw", Members: []string{"n1"}},
		galera.Node{Slot: "n1", NodeUUID: uuidA, HostID: "host-1", Volume: volume}, galera.SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := ServiceSpec{Name: gs.Name, Image: gs.Image, Labels: gs.Labels, Args: gs.Args, Constraints: gs.Constraints}
	for _, m := range gs.Mounts {
		s.Mounts = append(s.Mounts, Mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return s
}

func TestStartDBResolvesVolumeNameMount(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	wantStatus(t, f.mustDo(1, "op", "vol", KindCreateVolume, CreateVolumeArgs{Volume: "shop-n1", VolumeRoot: root}), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "init", KindInitIdentity, InitIdentityArgs{VolumeRoot: root,
		Token: NewIdentityToken{TokenID: "tok", Identity: testIdentity()}}), StatusSucceeded, "")
	created := f.mustDo(1, "op", "svc", KindCreateService, galeraSpec(t, "shop-n1"))
	wantStatus(t, created, StatusSucceeded, "")
	var out struct{ ID string }
	if err := json.Unmarshal(created.Output, &out); err != nil {
		t.Fatal(err)
	}
	if m := f.docker.specs[out.ID].Mounts; len(m) != 1 || m[0].Type != "volume" || m[0].Source != "shop-n1" {
		t.Fatalf("builder mount is not by volume name: %+v", m)
	}
	r := f.mustDo(1, "op", "start", KindStartDB, StartDBArgs{ServiceID: out.ID, VolumeRoot: root, Expected: testIdentity()})
	wantStatus(t, r, StatusSucceeded, "")
	if countPrefix(f.docker.Calls(), "scale-service "+out.ID+" 1") != 1 {
		t.Fatalf("service not scaled: %v", f.docker.Calls())
	}

	// Another node's root is not what the service mounts.
	other := f.volume("galera-1")
	writeMarker(t, other, testIdentity())
	wantStatus(t, f.mustDo(1, "op", "start-other", KindStartDB, StartDBArgs{ServiceID: out.ID, VolumeRoot: other, Expected: testIdentity()}),
		StatusRefused, CodeVolumeMismatch)
}

func TestStartDBVolumeMountMustBeOwned(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, root, testIdentity())
	own := map[string]string{LabelInstallation: testInstallation}
	// A foreign volume with the right mountpoint, and an owned service that
	// mounts it by name: the mount must not count.
	f.docker.objects["shop-n1"] = Object{Kind: KindVolume, ID: "shop-n1", Name: "shop-n1", Labels: map[string]string{LabelInstallation: "other"}, Mountpoint: root}
	f.docker.objects["svc-v"] = Object{Kind: KindService, ID: "svc-v", Labels: own, Mounts: []Mount{{Type: "volume", Source: "shop-n1", Target: "/dbcm"}}}
	f.docker.objects["svc-missing"] = Object{Kind: KindService, ID: "svc-missing", Labels: own, Mounts: []Mount{{Type: "volume", Source: "nope", Target: "/dbcm"}}}
	wantStatus(t, f.mustDo(1, "op", "allow", KindAllowObjects, AllowObjectsArgs{IDs: []string{"svc-v", "shop-n1", "svc-missing", "nope"}}), StatusSucceeded, "")
	wantStatus(t, f.mustDo(1, "op", "s1", KindStartDB, StartDBArgs{ServiceID: "svc-v", VolumeRoot: root, Expected: testIdentity()}), StatusRefused, CodeVolumeMismatch)
	wantStatus(t, f.mustDo(1, "op", "s2", KindStartDB, StartDBArgs{ServiceID: "svc-missing", VolumeRoot: root, Expected: testIdentity()}), StatusRefused, CodeVolumeMismatch)
	if len(f.docker.Mutations()) != 0 {
		t.Fatalf("mutations: %v", f.docker.Mutations())
	}
}

type fakeStatus struct {
	mu    sync.Mutex
	calls []string
	vars  map[string]string
	err   error
}

func (s *fakeStatus) Status(_ context.Context, network, addr string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, network+" "+addr)
	return s.vars, s.err
}

func (f *fixture) withStatus(s StatusQuerier) {
	f.t.Helper()
	g, err := fencing.NewGuard(fencing.NewFilePersister(f.termPath))
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := NewExecutor(Config{InstallationID: testInstallation, StatePath: f.statePath, VolumeBase: f.base,
		AllowedIDs: []string{"svc-1", "vol-1"}, Guard: g, Docker: f.docker, Status: s})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec = e
}

func TestCollectHealth(t *testing.T) {
	f := newFixture(t)
	st := &fakeStatus{vars: map[string]string{"wsrep_cluster_size": "3", "wsrep_ready": "ON", "Uptime": "5"}}
	f.withStatus(st)
	stateBefore := readFile(t, f.statePath)

	r := f.mustDo(4, "health", "h1", KindCollectHealth, CollectHealthArgs{Address: "127.0.0.1:3306"})
	wantStatus(t, r, StatusSucceeded, "")
	var out HealthOutput
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status["wsrep_cluster_size"] != "3" || out.Status["Uptime"] != "" {
		t.Fatalf("output %+v", out)
	}
	// Not recorded: the same step ID observes again.
	r = f.mustDo(4, "health", "h1", KindCollectHealth, CollectHealthArgs{Address: "127.0.0.1:3306"})
	if r.Replayed || len(st.calls) != 2 {
		t.Fatalf("read-only step replayed: %+v calls=%v", r, st.calls)
	}
	if string(readFile(t, f.statePath)) != string(stateBefore) {
		t.Fatal("read-only command changed the state file")
	}

	// Socket below the volume base.
	sock := filepath.Join(f.volume("galera-1"), "run", "mysqld.sock")
	writeRaw(t, sock, "")
	wantStatus(t, f.mustDo(4, "health", "h2", KindCollectHealth, CollectHealthArgs{Socket: sock}), StatusSucceeded, "")
	if got := st.calls[len(st.calls)-1]; !strings.HasPrefix(got, "unix ") || !strings.HasSuffix(got, "run/mysqld.sock") {
		t.Fatalf("socket call %q", got)
	}

	// Database errors are failures with a stable code.
	st.err = errors.New("connection refused")
	wantStatus(t, f.mustDo(4, "health", "h3", KindCollectHealth, CollectHealthArgs{Address: "[::1]:3306"}), StatusFailed, CodeDBUnavailable)

	// Fencing applies to reads too.
	if _, err := f.do(3, "health", "h4", KindCollectHealth, CollectHealthArgs{Address: "127.0.0.1:3306"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("stale read: %v", err)
	}
}

func TestCollectHealthArgsAndConfig(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.mustDo(1, "h", "nocfg", KindCollectHealth, CollectHealthArgs{Address: "127.0.0.1:3306"}), StatusRefused, CodeNotConfigured)
	f.withStatus(&fakeStatus{})
	outside := filepath.Join(f.dir, "mysqld.sock")
	for name, a := range map[string]CollectHealthArgs{
		"none":         {},
		"both":         {Address: "127.0.0.1:3306", Socket: filepath.Join(f.base, "x.sock")},
		"remote":       {Address: "10.0.0.5:3306"},
		"hostname":     {Address: "localhost:3306"},
		"no port":      {Address: "127.0.0.1"},
		"outside":      {Socket: outside},
		"relative":     {Socket: "x.sock"},
		"dot-dot path": {Socket: f.base + "/../x.sock"},
	} {
		if _, err := f.do(1, "h", "bad-"+name, KindCollectHealth, a); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A symlink below the base that points outside is refused at execution.
	link := filepath.Join(f.volume("galera-1"), "evil.sock")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "h", "dangling", KindCollectHealth, CollectHealthArgs{Socket: link}), StatusRefused, CodeInvalidPath)
	writeRaw(t, outside, "")
	wantStatus(t, f.mustDo(1, "h", "link", KindCollectHealth, CollectHealthArgs{Socket: link}), StatusRefused, CodeInvalidPath)
	wantStatus(t, f.mustDo(1, "h", "missing", KindCollectHealth, CollectHealthArgs{Socket: filepath.Join(f.base, "none.sock")}), StatusRefused, CodeInvalidPath)
}

const grastate = "# GALERA saved state\nversion: 2.1\nuuid:    6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f\nseqno:   1234\nsafe_to_bootstrap: 1\n"

func TestReadGrastate(t *testing.T) {
	f := newFixture(t)
	root := f.volume("galera-1")
	writeMarker(t, root, testIdentity())
	read := func(step string, a ReadGrastateArgs) (Result, GrastateOutput) {
		r := f.mustDo(1, "diag", step, KindReadGrastate, a)
		var out GrastateOutput
		if r.Status == StatusSucceeded {
			if err := json.Unmarshal(r.Output, &out); err != nil {
				t.Fatal(err)
			}
		}
		return r, out
	}
	args := ReadGrastateArgs{VolumeRoot: root, Expected: testIdentity()}

	r, out := read("missing", args)
	wantStatus(t, r, StatusSucceeded, "")
	if out.Present {
		t.Fatal("missing file reported present")
	}
	writeRaw(t, filepath.Join(root, "data", GrastateFileName), grastate)
	r, out = read("ok", args)
	wantStatus(t, r, StatusSucceeded, "")
	if !out.Present || out.Grastate == nil || out.Grastate.Seqno != 1234 || !out.Grastate.SafeToBootstrap {
		t.Fatalf("parsed %+v", out)
	}
	writeRaw(t, filepath.Join(root, "data", GrastateFileName), "garbage")
	_, out = read("bad", args)
	if !out.Present || out.Grastate != nil || out.ParseError == "" {
		t.Fatalf("unparsable grastate: %+v", out)
	}

	// Wrong identity: refused before reading.
	other := testIdentity()
	other.NodeUUID = uuidB
	r, _ = read("other", ReadGrastateArgs{VolumeRoot: root, Expected: other})
	wantStatus(t, r, StatusRefused, CodeIdentityMismatch)

	// Path confinement.
	if _, err := f.do(1, "diag", "dd", KindReadGrastate, ReadGrastateArgs{VolumeRoot: root, Expected: testIdentity(), DataDir: "../galera-2"}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("data_dir escape: %v", err)
	}
	if _, err := f.do(1, "diag", "abs", KindReadGrastate, ReadGrastateArgs{VolumeRoot: root, Expected: testIdentity(), DataDir: "/etc"}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("absolute data_dir: %v", err)
	}
	secret := filepath.Join(f.dir, "secret")
	writeRaw(t, filepath.Join(secret, GrastateFileName), grastate)
	if err := os.RemoveAll(filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	r, _ = read("symlink", args)
	wantStatus(t, r, StatusRefused, CodeInvalidPath)

	// Not a regular file.
	if err := os.Remove(filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data", GrastateFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ = read("dir", args)
	wantStatus(t, r, StatusRefused, CodeInvalidPath)

	// Too large.
	if err := os.RemoveAll(filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, "data", GrastateFileName), strings.Repeat("#", maxGrastateSize+1))
	r, _ = read("large", args)
	wantStatus(t, r, StatusFailed, CodeIO)
}
