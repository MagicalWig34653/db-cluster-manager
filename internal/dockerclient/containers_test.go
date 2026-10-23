// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

func frame(stream byte, s string) string {
	var h [8]byte
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return string(h[:]) + s
}

func TestDemuxLogs(t *testing.T) {
	in := frame(1, "out line\n") + frame(2, "err line\n") + frame(1, "trunc")[:12]
	got, err := demuxLogs(strings.NewReader(in))
	if err != nil || string(got) != "out line\nerr line\ntrun" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRunContainerLifecycle(t *testing.T) {
	f := newFakeDaemon(t)
	id := "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	f.route("POST /v1.47/containers/create", reply{status: 201, body: `{"Id":"` + id + `","Warnings":[]}`})
	f.route("POST /v1.47/containers/"+id+"/start", reply{status: 204})
	f.route("POST /v1.47/containers/"+id+"/wait", reply{status: 200, body: `{"StatusCode":0}`})
	f.route("GET /v1.47/containers/"+id+"/logs", reply{status: 200, body: frame(2, "WSREP: Recovered position: u:5\n"),
		header: map[string]string{"Content-Type": "application/vnd.docker.multiplexed-stream"}})
	f.route("DELETE /v1.47/containers/"+id, reply{status: 204})
	c := f.client(Options{})
	res, err := c.RunContainer(context.Background(), agent.ContainerRun{Image: "mariadb:11.4", Entrypoint: []string{"mariadbd"},
		Cmd: []string{"--wsrep-recover"}, User: "999:999", Mounts: []agent.Mount{{Type: "volume", Source: "v1", Target: "/dbcm"}}})
	if err != nil || res.ExitCode != 0 || !bytes.Contains(res.Output, []byte("Recovered position")) {
		t.Fatalf("res %+v, %v", res, err)
	}
	var create map[string]any
	var paths []string
	for _, r := range f.requests() {
		paths = append(paths, r.Method+" "+r.Path)
		if r.Path == "/v1.47/containers/create" {
			_ = json.Unmarshal([]byte(r.Body), &create)
		}
	}
	hc, _ := create["HostConfig"].(map[string]any)
	if create["NetworkDisabled"] != true || hc["NetworkMode"] != "none" || create["User"] != "999:999" {
		t.Fatalf("create body %v", create)
	}
	if last := paths[len(paths)-1]; last != "DELETE /v1.47/containers/"+id {
		t.Fatalf("container not removed last: %v", paths)
	}
	for _, r := range f.requests() {
		if r.Method == "DELETE" && strings.Contains(r.Query, "v=") {
			t.Fatalf("remove touches volumes: %+v", r)
		}
	}
}

func TestRunContainerRemovesAfterFailedStart(t *testing.T) {
	f := newFakeDaemon(t)
	id := "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee01"
	f.route("POST /v1.47/containers/create", reply{status: 201, body: `{"Id":"` + id + `"}`})
	f.route("POST /v1.47/containers/"+id+"/start", reply{status: 500, body: `{"message":"boom"}`})
	f.route("DELETE /v1.47/containers/"+id, reply{status: 204})
	c := f.client(Options{})
	if _, err := c.RunContainer(context.Background(), agent.ContainerRun{Image: "x", Entrypoint: []string{"y"}}); err == nil {
		t.Fatal("start failure not reported")
	}
	if r := f.last(); r.Method != "DELETE" {
		t.Fatalf("last request %+v", r)
	}
}

func TestRunningContainersWithVolume(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/containers/json", reply{status: 200, body: cannedContainerList})
	c := f.client(Options{})
	ids, err := c.RunningContainersWithVolume(context.Background(), "v1")
	if err != nil || len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("ids %v, %v", ids, err)
	}
	if q := f.last().Query; !strings.Contains(q, "volume") || !strings.Contains(q, "running") {
		t.Fatalf("query %q", q)
	}
}
