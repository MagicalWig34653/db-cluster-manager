// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

func TestParseFlags(t *testing.T) {
	base := []string{"-listen", "127.0.0.1:7070", "-state", "/var/lib/a", "-installation", "inst-1", "-volume-base", "/srv/dbcm"}
	o, err := parseFlags(base)
	if err != nil {
		t.Fatal(err)
	}
	if o.listen != "127.0.0.1:7070" || o.docker != "unix:///var/run/docker.sock" || o.stateDir != "/var/lib/a" || o.installation != "inst-1" || o.volumeBase != "/srv/dbcm" {
		t.Fatalf("parsed %+v", o)
	}
	if o, err := parseFlags([]string{"-version"}); err != nil || !o.showVersion {
		t.Fatalf("-version: %+v, %v", o, err)
	}
	if _, err := parseFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v", err)
	}
	with := func(k, v string) []string {
		out := append([]string{}, base...)
		for i := 0; i < len(out); i += 2 {
			if out[i] == k {
				out[i+1] = v
				return out
			}
		}
		return append(out, k, v)
	}
	bad := map[string][]string{
		"missing listen":       base[2:],
		"missing state":        with("-state", ""),
		"missing installation": with("-installation", ""),
		"missing volume base":  with("-volume-base", ""),
		"non-loopback listen":  with("-listen", "0.0.0.0:7070"),
		"hostname listen":      with("-listen", "localhost:7070"),
		"listen without port":  with("-listen", "127.0.0.1"),
		"relative state":       with("-state", "state"),
		"relative volume base": with("-volume-base", "srv"),
		"unclean volume base":  with("-volume-base", "/srv/../srv/dbcm"),
		"stray argument":       append(append([]string{}, base...), "extra"),
		"unknown flag":         append(append([]string{}, base...), "-nope"),
	}
	for name, args := range bad {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: got nil error", name)
		}
	}
}

// fakeDocker is a minimal Docker Engine API stand-in on a unix socket with
// canned responses in the shape of the v1.47 reference.
type fakeDocker struct {
	mu   sync.Mutex
	reqs []string
	body map[string]string // last request body per "METHOD path"
	sock string
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeDocker{sock: filepath.Join(dir, "d.sock"), body: map[string]string{}}
	l, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.reqs = append(f.reqs, key)
		f.body[key] = string(b)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "GET /version":
			_, _ = io.WriteString(w, `{"Version":"27.0.1","ApiVersion":"1.47","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`)
		case "POST /v1.47/services/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ID":"ak7w3gjqoa3kuz8xcpnyy0pvl","Warnings":[]}`)
		case "GET /v1.47/services/ak7w3gjqoa3kuz8xcpnyy0pvl":
			_, _ = io.WriteString(w, `{"ID":"ak7w3gjqoa3kuz8xcpnyy0pvl","Version":{"Index":5},"Spec":{"Name":"db-a",`+
				`"Labels":{"dbcm.installation":"inst-1"},"TaskTemplate":{"ContainerSpec":{"Image":"postgres:17"}},"Mode":{"Replicated":{"Replicas":0}}}}`)
		case "DELETE /v1.47/services/ak7w3gjqoa3kuz8xcpnyy0pvl":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"page not found"}`)
		}
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return f
}

func (f *fakeDocker) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func startAgent(t *testing.T, docker string) (string, func() error) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	volBase := t.TempDir()
	o, err := parseFlags([]string{"-listen", "127.0.0.1:0", "-docker", docker, "-state", state, "-installation", "inst-1", "-volume-base", volBase})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, o, io.Discard, func(a string) { addrc <- a }) }()
	select {
	case a := <-addrc:
		return a, func() error {
			cancel()
			return <-errc
		}
	case err := <-errc:
		cancel()
		t.Fatalf("run: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("agent did not start")
	}
	return "", nil
}

func TestRunEndToEndWithFakeDocker(t *testing.T) {
	fd := newFakeDocker(t)
	addr, stop := startAgent(t, "unix://"+fd.sock)
	lc := &agent.LabClient{BaseURL: "http://" + addr}
	ctx := context.Background()

	spec, _ := json.Marshal(agent.ServiceSpec{Name: "db-a", Image: "postgres:17", Constraints: []string{"node.id==n1"}, Replicas: 0})
	res, err := lc.Handle(ctx, agent.Command{Term: 1, OpID: "op1", StepID: "s1", Kind: agent.KindCreateService, Args: spec})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.StatusSucceeded || !strings.Contains(string(res.Output), "ak7w3gjqoa3kuz8xcpnyy0pvl") {
		t.Fatalf("create result = %+v", res)
	}
	fd.mu.Lock()
	created := fd.body["POST /v1.47/services/create"]
	fd.mu.Unlock()
	var wire struct {
		Labels       map[string]string
		TaskTemplate struct {
			Placement struct{ Constraints []string }
		}
	}
	if err := json.Unmarshal([]byte(created), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Labels[agent.LabelInstallation] != "inst-1" || wire.Labels[agent.LabelOpID] != "op1" || wire.Labels[agent.LabelStepID] != "s1" ||
		len(wire.TaskTemplate.Placement.Constraints) != 1 {
		t.Fatalf("create body = %s", created)
	}

	sa, _ := json.Marshal(agent.ServiceArgs{ServiceID: "ak7w3gjqoa3kuz8xcpnyy0pvl"})
	res, err = lc.Handle(ctx, agent.Command{Term: 1, OpID: "op1", StepID: "s2", Kind: agent.KindRemoveService, Args: sa})
	if err != nil || res.Status != agent.StatusSucceeded {
		t.Fatalf("remove = %+v, %v", res, err)
	}

	// A service outside the allow-list is refused without any Docker call.
	before := len(fd.requests())
	other, _ := json.Marshal(agent.ServiceArgs{ServiceID: "foreign"})
	res, err = lc.Handle(ctx, agent.Command{Term: 1, OpID: "op1", StepID: "s3", Kind: agent.KindRemoveService, Args: other})
	if err != nil || res.Status != agent.StatusRefused || res.Code != agent.CodeNotOwned {
		t.Fatalf("foreign remove = %+v, %v", res, err)
	}
	if after := len(fd.requests()); after != before {
		t.Fatalf("Docker called for a foreign ID: %v", fd.requests()[before:])
	}

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := []string{"GET /version", "POST /v1.47/services/create", "GET /v1.47/services/ak7w3gjqoa3kuz8xcpnyy0pvl", "DELETE /v1.47/services/ak7w3gjqoa3kuz8xcpnyy0pvl"}
	if got := fd.requests(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("docker requests = %v, want %v", got, want)
	}
}

func TestRunFailsWithoutDocker(t *testing.T) {
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	o, err := parseFlags([]string{"-listen", "127.0.0.1:0", "-docker", "unix://" + filepath.Join(dir, "none.sock"),
		"-state", filepath.Join(dir, "state"), "-installation", "inst-1", "-volume-base", dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), o, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("run = %v, want docker error", err)
	}
}

func TestRunRejectsMissingVolumeBase(t *testing.T) {
	dir := t.TempDir()
	// Never the real daemon socket: point -docker at a path that does not exist.
	o, err := parseFlags([]string{"-listen", "127.0.0.1:0", "-docker", "unix://" + filepath.Join(dir, "none.sock"),
		"-state", filepath.Join(dir, "state"), "-installation", "i", "-volume-base", filepath.Join(dir, "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), o, io.Discard, nil); err == nil {
		t.Fatal("run accepted a missing volume base")
	}
}
