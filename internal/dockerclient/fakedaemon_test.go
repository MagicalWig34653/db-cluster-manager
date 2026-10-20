// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Canned responses. Unless marked "abridged", they are the examples of the
// Docker Engine API reference v1.47 (v1.47.yaml, definitions Service,
// SystemVersion, ServiceCreateResponse, Volume). Abridged responses keep
// only documented field names with plausible values.
const (
	cannedVersion = `{"Platform":{"Name":"Docker Engine - Community"},"Components":[{"Name":"Engine","Version":"27.0.1"}],` +
		`"Version":"27.0.1","ApiVersion":"1.47","MinAPIVersion":"1.24","GitCommit":"48a66213fe","GoVersion":"go1.21.13",` +
		`"Os":"linux","Arch":"amd64","KernelVersion":"6.8.0-31-generic","Experimental":true,"BuildTime":"2020-06-22T15:49:27.000000000+00:00"}`

	cannedServiceCreate = `{"ID":"ak7w3gjqoa3kuz8xcpnyy0pvl","Warnings":["unable to pin image doesnotexist:latest to digest: image library/doesnotexist:latest not found"]}`

	cannedService = `{"ID":"9mnpnzenvg8p8tdbtq4wvbkcz","Version":{"Index":19},"CreatedAt":"2016-06-07T21:05:51.880065305Z",` +
		`"UpdatedAt":"2016-06-07T21:07:29.962229872Z","Spec":{"Name":"hopeful_cori","TaskTemplate":{"ContainerSpec":{"Image":"redis"},` +
		`"Resources":{"Limits":{},"Reservations":{}},"RestartPolicy":{"Condition":"any","MaxAttempts":0},"Placement":{},"ForceUpdate":0},` +
		`"Mode":{"Replicated":{"Replicas":1}},"UpdateConfig":{"Parallelism":1,"Delay":1000000000,"FailureAction":"pause","Monitor":15000000000,"MaxFailureRatio":0.15},` +
		`"RollbackConfig":{"Parallelism":1,"Delay":1000000000,"FailureAction":"pause","Monitor":15000000000,"MaxFailureRatio":0.15},` +
		`"EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":6379,"PublishedPort":30001}]}},` +
		`"Endpoint":{"Spec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":6379,"PublishedPort":30001}]},` +
		`"Ports":[{"Protocol":"tcp","TargetPort":6379,"PublishedPort":30001}],` +
		`"VirtualIPs":[{"NetworkID":"4qvuz4ko70xaltuqbt8956gd1","Addr":"10.255.0.2/16"},{"NetworkID":"4qvuz4ko70xaltuqbt8956gd1","Addr":"10.255.0.3/16"}]}}`

	// cannedOwnedService is cannedService with ownership labels and a bind
	// mount added (abridged otherwise).
	cannedOwnedService = `{"ID":"svc1","Version":{"Index":7},"Spec":{"Name":"db-a","Labels":{"dbcm.installation":"inst-1","dbcm.node":"n1"},` +
		`"TaskTemplate":{"ContainerSpec":{"Image":"postgres:17","Mounts":[{"Type":"bind","Source":"/srv/dbcm/vol1","Target":"/var/lib/postgresql/data"},` +
		`{"Type":"volume","Source":"cfg","Target":"/etc/cfg","ReadOnly":true}]}},"Mode":{"Replicated":{"Replicas":0}}}}`

	cannedGlobalService = `{"ID":"svcg","Version":{"Index":3},"Spec":{"Name":"agent","TaskTemplate":{"ContainerSpec":{"Image":"x"}},"Mode":{"Global":{}}}}`

	cannedVolume = `{"Name":"tardis","Driver":"custom","Mountpoint":"/var/lib/docker/volumes/tardis","CreatedAt":"2016-06-07T20:31:11.853781916Z",` +
		`"Status":{"hello":"world"},"Labels":{"com.example.some-label":"some-value","com.example.some-other-label":"some-other-value"},` +
		`"Scope":"local","Options":{"device":"tmpfs","o":"size=100m,uid=1000","type":"tmpfs"}}`

	// abridged
	cannedVolumeList = `{"Volumes":[{"Name":"v1","Driver":"local","Mountpoint":"/srv/dbcm/v1","Labels":{"dbcm.installation":"inst-1"},"Scope":"local"},` +
		`{"Name":"v2","Driver":"local","Mountpoint":"/srv/dbcm/v2","Labels":{"dbcm.installation":"other"},"Scope":"local"}],"Warnings":[]}`
	cannedContainer = `{"Id":"aa86eacfb3b3ed4cd362c1e88fc89a53908ad05fb3a4103bca3f9b28292d14bf","Name":"/db-a.1.xyz","Image":"sha256:abc",` +
		`"Config":{"Image":"postgres:17","Labels":{"dbcm.installation":"inst-1","com.docker.swarm.service.id":"svc1"}}}`
	cannedContainerList = `[{"Id":"c1","Names":["/db-a.1.xyz"],"Image":"postgres:17","Labels":{"dbcm.installation":"inst-1"},"State":"running"}]`
	cannedNetwork       = `{"Name":"dbcm-net","Id":"7d86d31b1478e7cca9ebed7e73aa0fdeec46c5ca29497431d3007d2d9e15ed99","Scope":"swarm","Driver":"overlay","Labels":{"dbcm.installation":"inst-1"}}`
	cannedSecret        = `{"ID":"ktnbjxoalbkvbvedmg1urrz8h","Version":{"Index":11},"Spec":{"Name":"app-dev.crt","Labels":{"dbcm.installation":"inst-1"}}}`
	cannedConfigList    = `[{"ID":"ktnbjxoalbkvbvedmg1urrz8h","Version":{"Index":11},"Spec":{"Name":"server.conf","Labels":{"dbcm.installation":"inst-1"}}}]`
)

type recorded struct {
	Method, Path, Query, Body, ContentType string
}

type reply struct {
	status int
	body   string
	header map[string]string
	// block, if set, makes the handler wait for the client to go away.
	block bool
}

// fakeDaemon is an httptest server on a unix socket that records requests
// and answers from a route table keyed by "METHOD /path".
type fakeDaemon struct {
	t      *testing.T
	srv    *httptest.Server
	sock   string
	mu     sync.Mutex
	reqs   []recorded
	routes map[string]reply
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// Unix socket paths are limited to about 104 bytes on macOS; t.TempDir
	// can exceed that.
	dir, err := os.MkdirTemp("", "dc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{t: t, sock: sock, routes: map[string]reply{
		"GET /version": {status: 200, body: cannedVersion},
		"GET /_ping":   {status: 200, body: "OK", header: map[string]string{"Api-Version": "1.47", "Swarm": "active/manager"}},
	}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Listener = l
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body), ContentType: r.Header.Get("Content-Type")})
	rep, ok := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"page not found"}`)
		return
	}
	if rep.block {
		<-r.Context().Done()
		return
	}
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (f *fakeDaemon) route(key string, rep reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = rep
}

func (f *fakeDaemon) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// last returns the last recorded request other than the version handshake.
func (f *fakeDaemon) last() recorded {
	f.t.Helper()
	rs := f.requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Path != "/version" {
			return rs[i]
		}
	}
	f.t.Fatal("no request recorded")
	return recorded{}
}

func (f *fakeDaemon) client(o Options) *Client {
	f.t.Helper()
	o.Host = "unix://" + f.sock
	c, err := New(o)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(c.Close)
	return c
}
