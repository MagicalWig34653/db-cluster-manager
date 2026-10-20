// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

func jsonEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestNewHost(t *testing.T) {
	for _, ok := range []string{"", "unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "tcp://[::1]:2375"} {
		if _, err := New(Options{Host: ok}); err != nil {
			t.Errorf("New(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"unix://relative/sock", "unix://", "tcp://", "tcp://host", "tcp://h:1/x", "http://127.0.0.1:2375", "npipe:////./pipe/docker_engine", "::"} {
		if _, err := New(Options{Host: bad}); err == nil {
			t.Errorf("New(%q): got nil error", bad)
		}
	}
	if _, err := New(Options{APIVersion: "v1"}); err == nil {
		t.Error("malformed pinned version accepted")
	}
}

func TestNegotiateTable(t *testing.T) {
	cases := []struct {
		server, serverMin, want string
	}{
		{"1.47", "1.24", "1.47"},  // Docker 27.x
		{"1.51", "1.24", "1.47"},  // newer daemon: use our maximum
		{"1.52", "1.44", "1.47"},  // Docker 29 style minimum
		{"1.43", "1.12", "1.43"},  // older daemon within range
		{"1.41", "", "1.41"},      // our minimum, no daemon minimum reported
		{"1.100", "1.24", "1.47"}, // numeric, not lexical, comparison
		{"2.0", "1.24", "1.47"},
		{"1.40", "1.12", ""}, // older than MinAPIVersion
		{"1.5", "1.2", ""},   // 1.5 < 1.41
		{"1.60", "1.50", ""}, // daemon minimum above our maximum
		{"1.45", "1.46", ""}, // inconsistent daemon range
		{"", "", ""},         // no version reported
		{"one.two", "", ""},  // malformed
		{"1.47.0", "", ""},   // three components
		{"1", "", ""},        // one component
		{"-1.47", "", ""},    // negative
		{"1.47 ", "", ""},    // whitespace
		{"1.47", "x", ""},    // malformed minimum
	}
	for _, c := range cases {
		got, err := negotiate(c.server, c.serverMin)
		if c.want == "" {
			if err == nil || !errors.Is(err, ErrAPIVersion) {
				t.Errorf("negotiate(%q,%q) = %q, %v; want ErrAPIVersion", c.server, c.serverMin, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("negotiate(%q,%q) = %q, %v; want %q", c.server, c.serverMin, got, err, c.want)
		}
	}
}

func TestNegotiationOverSocket(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /version", reply{status: 200, body: strings.Replace(cannedVersion, `"ApiVersion":"1.47"`, `"ApiVersion":"1.45"`, 1)})
	f.route("GET /v1.45/services", reply{status: 200, body: `[]`})
	c := f.client(Options{})
	if c.APIVersion() != "" {
		t.Fatal("version set before negotiation")
	}
	if _, err := c.ListByLabel(context.Background(), agent.KindService, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListByLabel(context.Background(), agent.KindService, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if c.APIVersion() != "1.45" {
		t.Fatalf("APIVersion = %q", c.APIVersion())
	}
	var versionCalls int
	for _, r := range f.requests() {
		if r.Path == "/version" {
			versionCalls++
		}
	}
	if versionCalls != 1 {
		t.Fatalf("negotiated %d times, want once", versionCalls)
	}

	ping, err := c.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ping.APIVersion != "1.47" || ping.Swarm != "active/manager" {
		t.Fatalf("ping = %+v", ping)
	}
	v, err := c.Version(context.Background())
	if err != nil || v.Version != "27.0.1" || v.MinAPIVersion != "1.24" || v.Os != "linux" {
		t.Fatalf("version = %+v, %v", v, err)
	}
}

func TestNegotiationFailureIsNotCached(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /version", reply{status: 200, body: `{"ApiVersion":"1.30","MinAPIVersion":"1.12"}`})
	c := f.client(Options{})
	_, err := c.Inspect(context.Background(), agent.KindVolume, "v1")
	if !errors.Is(err, ErrAPIVersion) {
		t.Fatalf("err = %v, want ErrAPIVersion", err)
	}
	// Only the handshake may have been sent.
	for _, r := range f.requests() {
		if r.Path != "/version" {
			t.Fatalf("request %s %s sent despite failed negotiation", r.Method, r.Path)
		}
	}
	f.route("GET /version", reply{status: 200, body: cannedVersion})
	f.route("GET /v1.47/volumes/v1", reply{status: 200, body: cannedVolume})
	if _, err := c.Inspect(context.Background(), agent.KindVolume, "v1"); err != nil {
		t.Fatalf("after daemon upgrade: %v", err)
	}
}

func TestPinnedVersionSkipsNegotiation(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.44/volumes/tardis", reply{status: 200, body: cannedVolume})
	c := f.client(Options{APIVersion: "1.44"})
	if _, err := c.Inspect(context.Background(), agent.KindVolume, "tardis"); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.requests() {
		if r.Path == "/version" {
			t.Fatal("pinned client negotiated")
		}
	}
}

func TestCreateServiceRequest(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	c := f.client(Options{})
	spec := agent.ServiceSpec{
		Name:        "db-a",
		Image:       "postgres:17",
		Labels:      map[string]string{agent.LabelInstallation: "inst-1", agent.LabelNode: "n1"},
		Env:         []string{"PGDATA=/var/lib/postgresql/data/pg"},
		Command:     []string{"docker-entrypoint.sh"},
		Args:        []string{"postgres", "-c", "fsync=on"},
		Mounts:      []agent.Mount{{Type: "bind", Source: "/srv/dbcm/vol1", Target: "/var/lib/postgresql/data"}, {Type: "volume", Source: "cfg", Target: "/etc/cfg", ReadOnly: true}},
		Constraints: []string{"node.id==2ug8d0gvofqzdn0vwwzt8tmyc"},
		Replicas:    1,
		RestartPolicy: &agent.RestartPolicy{
			Condition: "on-failure", DelayNS: 5_000_000_000, MaxAttempts: 3, WindowNS: 60_000_000_000,
		},
		Resources: &agent.Resources{
			Limits:       &agent.ResourceLimits{NanoCPUs: 2_000_000_000, MemoryBytes: 1 << 30, Pids: 512},
			Reservations: &agent.ResourceReservations{NanoCPUs: 500_000_000, MemoryBytes: 256 << 20},
		},
	}
	id, err := c.CreateService(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ak7w3gjqoa3kuz8xcpnyy0pvl" {
		t.Fatalf("id = %q", id)
	}
	r := f.last()
	if r.Method != "POST" || r.Path != "/v1.47/services/create" || r.ContentType != "application/json" {
		t.Fatalf("request = %+v", r)
	}
	jsonEqual(t, r.Body, `{
		"Name": "db-a",
		"Labels": {"dbcm.installation": "inst-1", "dbcm.node": "n1"},
		"TaskTemplate": {
			"ContainerSpec": {
				"Image": "postgres:17",
				"Labels": {"dbcm.installation": "inst-1", "dbcm.node": "n1"},
				"Command": ["docker-entrypoint.sh"],
				"Args": ["postgres", "-c", "fsync=on"],
				"Env": ["PGDATA=/var/lib/postgresql/data/pg"],
				"Mounts": [
					{"Type": "bind", "Source": "/srv/dbcm/vol1", "Target": "/var/lib/postgresql/data"},
					{"Type": "volume", "Source": "cfg", "Target": "/etc/cfg", "ReadOnly": true}
				]
			},
			"Resources": {
				"Limits": {"NanoCPUs": 2000000000, "MemoryBytes": 1073741824, "Pids": 512},
				"Reservations": {"NanoCPUs": 500000000, "MemoryBytes": 268435456}
			},
			"RestartPolicy": {"Condition": "on-failure", "Delay": 5000000000, "MaxAttempts": 3, "Window": 60000000000},
			"Placement": {"Constraints": ["node.id==2ug8d0gvofqzdn0vwwzt8tmyc"]}
		},
		"Mode": {"Replicated": {"Replicas": 1}}
	}`)
}

func TestCreateServiceMinimalSendsZeroReplicas(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	c := f.client(Options{})
	if _, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "busybox", Replicas: 0}); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, f.last().Body, `{"Name":"s","TaskTemplate":{"ContainerSpec":{"Image":"busybox"}},"Mode":{"Replicated":{"Replicas":0}}}`)
}

func TestCreateServiceValidation(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{})
	bad := []agent.ServiceSpec{
		{Image: "x"},
		{Name: "x"},
		{Name: "x", Image: "x", Mounts: []agent.Mount{{Type: "bind", Source: "/a"}}},
		{Name: "x", Image: "x", Mounts: []agent.Mount{{Type: "tmpfs", Target: "/a"}}},
		{Name: "x", Image: "x", RestartPolicy: &agent.RestartPolicy{Condition: "always"}},
	}
	for _, s := range bad {
		if _, err := c.CreateService(context.Background(), s); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("spec %+v: err = %v, want ErrInvalidArgument", s, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent for invalid specs", n)
	}
}

func TestScaleServicePreservesSpec(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz", reply{status: 200, body: cannedService})
	f.route("POST /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz/update", reply{status: 200, body: `{"Warnings":[]}`})
	c := f.client(Options{})
	if err := c.ScaleService(context.Background(), "9mnpnzenvg8p8tdbtq4wvbkcz", 0); err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "POST" || r.Query != "version=19" {
		t.Fatalf("update request = %+v", r)
	}
	var full map[string]json.RawMessage
	_ = json.Unmarshal([]byte(cannedService), &full)
	var spec map[string]any
	_ = json.Unmarshal(full["Spec"], &spec)
	spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 0}}
	want, _ := json.Marshal(spec)
	jsonEqual(t, r.Body, string(want))
}

func TestScaleGlobalServiceRefused(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/svcg", reply{status: 200, body: cannedGlobalService})
	c := f.client(Options{})
	if err := c.ScaleService(context.Background(), "svcg", 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if r := f.last(); r.Method != "GET" {
		t.Fatalf("unexpected %s %s", r.Method, r.Path)
	}
}

func TestRemove(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("DELETE /v1.47/services/svc1", reply{status: 200})
	f.route("DELETE /v1.47/volumes/v1", reply{status: 204})
	c := f.client(Options{})
	if err := c.RemoveService(context.Background(), "svc1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveVolume(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.Method != "DELETE" || r.Path != "/v1.47/volumes/v1" || r.Query != "" {
		t.Fatalf("request = %+v (force must not be set)", r)
	}
}

func TestInspectKinds(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/svc1", reply{status: 200, body: cannedOwnedService})
	f.route("GET /v1.47/volumes/tardis", reply{status: 200, body: cannedVolume})
	f.route("GET /v1.47/containers/c1/json", reply{status: 200, body: cannedContainer})
	f.route("GET /v1.47/networks/n1", reply{status: 200, body: cannedNetwork})
	f.route("GET /v1.47/secrets/s1", reply{status: 200, body: cannedSecret})
	f.route("GET /v1.47/configs/s1", reply{status: 200, body: cannedSecret})
	c := f.client(Options{})
	ctx := context.Background()

	svc, err := c.Inspect(ctx, agent.KindService, "svc1")
	if err != nil {
		t.Fatal(err)
	}
	want := agent.Object{Kind: agent.KindService, ID: "svc1", Name: "db-a",
		Labels: map[string]string{"dbcm.installation": "inst-1", "dbcm.node": "n1"},
		Mounts: []agent.Mount{{Type: "bind", Source: "/srv/dbcm/vol1", Target: "/var/lib/postgresql/data"}, {Type: "volume", Source: "cfg", Target: "/etc/cfg", ReadOnly: true}}}
	if !reflect.DeepEqual(svc, want) {
		t.Fatalf("service = %+v", svc)
	}
	vol, err := c.Inspect(ctx, agent.KindVolume, "tardis")
	if err != nil || vol.ID != "tardis" || vol.Mountpoint != "/var/lib/docker/volumes/tardis" || vol.Labels["com.example.some-label"] != "some-value" || vol.Kind != agent.KindVolume {
		t.Fatalf("volume = %+v, %v", vol, err)
	}
	ctr, err := c.Inspect(ctx, agent.KindContainer, "c1")
	if err != nil || ctr.Name != "db-a.1.xyz" || ctr.Labels["dbcm.installation"] != "inst-1" || !strings.HasPrefix(ctr.ID, "aa86") {
		t.Fatalf("container = %+v, %v", ctr, err)
	}
	nw, err := c.Inspect(ctx, agent.KindNetwork, "n1")
	if err != nil || nw.Name != "dbcm-net" || nw.Labels["dbcm.installation"] != "inst-1" {
		t.Fatalf("network = %+v, %v", nw, err)
	}
	for _, k := range []agent.ObjectKind{agent.KindSecret, agent.KindConfig} {
		o, err := c.Inspect(ctx, k, "s1")
		if err != nil || o.Kind != k || o.ID != "ktnbjxoalbkvbvedmg1urrz8h" || o.Name != "app-dev.crt" || o.Labels["dbcm.installation"] != "inst-1" {
			t.Fatalf("%s = %+v, %v", k, o, err)
		}
	}
	if _, err := c.Inspect(ctx, "plugin", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestInvalidIDsSendNothing(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{APIVersion: "1.47"})
	ctx := context.Background()
	for _, id := range []string{"", ".", "..", "../x", "a/b", "-x", "a?b", "a b", strings.Repeat("a", 300)} {
		if _, err := c.Inspect(ctx, agent.KindService, id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Inspect(%q): %v", id, err)
		}
		if err := c.RemoveService(ctx, id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("RemoveService(%q): %v", id, err)
		}
		if err := c.RemoveVolume(ctx, id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("RemoveVolume(%q): %v", id, err)
		}
		if err := c.ScaleService(ctx, id, 1); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ScaleService(%q): %v", id, err)
		}
	}
	if _, err := c.ListByLabel(ctx, agent.KindService, "a=b", "c"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("label key with '=': %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestListByLabel(t *testing.T) {
	f := newFakeDaemon(t)
	// The daemon "ignores" the filter for services: the client must drop the
	// foreign and the unlabelled service.
	f.route("GET /v1.47/services", reply{status: 200, body: `[` + cannedOwnedService + `,` + cannedService + `,` +
		strings.Replace(cannedOwnedService, `"inst-1"`, `"other"`, 1) + `]`})
	f.route("GET /v1.47/volumes", reply{status: 200, body: cannedVolumeList})
	f.route("GET /v1.47/containers/json", reply{status: 200, body: cannedContainerList})
	f.route("GET /v1.47/networks", reply{status: 200, body: `[` + cannedNetwork + `]`})
	f.route("GET /v1.47/secrets", reply{status: 200, body: `[]`})
	f.route("GET /v1.47/configs", reply{status: 200, body: cannedConfigList})
	c := f.client(Options{})
	ctx := context.Background()
	wantFilter := `{"label":["dbcm.installation=inst-1"]}`

	svcs, err := c.ListByLabel(ctx, agent.KindService, agent.LabelInstallation, "inst-1")
	if err != nil || len(svcs) != 1 || svcs[0].ID != "svc1" {
		t.Fatalf("services = %+v, %v", svcs, err)
	}
	checkFilter := func(path string, extra url.Values) {
		t.Helper()
		r := f.last()
		if r.Path != path {
			t.Fatalf("path = %q, want %q", r.Path, path)
		}
		q, _ := url.ParseQuery(r.Query)
		jsonEqual(t, q.Get("filters"), wantFilter)
		for k := range extra {
			if q.Get(k) != extra.Get(k) {
				t.Fatalf("query %s = %q, want %q", k, q.Get(k), extra.Get(k))
			}
		}
	}
	checkFilter("/v1.47/services", nil)

	vols, err := c.ListByLabel(ctx, agent.KindVolume, agent.LabelInstallation, "inst-1")
	if err != nil || len(vols) != 1 || vols[0].ID != "v1" || vols[0].Mountpoint != "/srv/dbcm/v1" {
		t.Fatalf("volumes = %+v, %v", vols, err)
	}
	checkFilter("/v1.47/volumes", nil)

	ctrs, err := c.ListByLabel(ctx, agent.KindContainer, agent.LabelInstallation, "inst-1")
	if err != nil || len(ctrs) != 1 || ctrs[0].Name != "db-a.1.xyz" {
		t.Fatalf("containers = %+v, %v", ctrs, err)
	}
	checkFilter("/v1.47/containers/json", url.Values{"all": {"1"}})

	for _, k := range []agent.ObjectKind{agent.KindNetwork, agent.KindSecret, agent.KindConfig} {
		if _, err := c.ListByLabel(ctx, k, agent.LabelInstallation, "inst-1"); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		checkFilter("/v1.47/"+string(k)+"s", nil)
	}
	empty, err := c.ListByLabel(ctx, agent.KindSecret, agent.LabelInstallation, "inst-1")
	if err != nil || len(empty) != 0 {
		t.Fatalf("secrets = %+v, %v", empty, err)
	}
	// A null volume list is an empty result.
	f.route("GET /v1.47/volumes", reply{status: 200, body: `{"Volumes":null,"Warnings":null}`})
	if vs, err := c.ListByLabel(ctx, agent.KindVolume, agent.LabelInstallation, "inst-1"); err != nil || len(vs) != 0 {
		t.Fatalf("null volumes = %+v, %v", vs, err)
	}
}

func TestErrorMapping(t *testing.T) {
	type check struct {
		name                                     string
		run                                      func(c *Client) error
		route                                    string
		status                                   int
		body                                     string
		notFound, conflict, server, unknown, bad bool
		unavailable                              bool
		message                                  string
	}
	ctx := context.Background()
	inspect := func(c *Client) error { _, err := c.Inspect(ctx, agent.KindService, "svc1"); return err }
	create := func(c *Client) error {
		_, err := c.CreateService(ctx, agent.ServiceSpec{Name: "s", Image: "i", Replicas: 1})
		return err
	}
	rmVol := func(c *Client) error { return c.RemoveVolume(ctx, "v1") }
	rmSvc := func(c *Client) error { return c.RemoveService(ctx, "svc1") }
	cases := []check{
		{name: "inspect 404", run: inspect, route: "GET /v1.47/services/svc1", status: 404,
			body: `{"message":"service svc1 not found"}`, notFound: true, message: "service svc1 not found"},
		{name: "inspect 500", run: inspect, route: "GET /v1.47/services/svc1", status: 500,
			body: `{"message":"rpc error"}`, server: true},
		{name: "inspect 503", run: inspect, route: "GET /v1.47/services/svc1", status: 503,
			body: `{"message":"This node is not a swarm manager."}`, server: true, unavailable: true},
		{name: "create 409", run: create, route: "POST /v1.47/services/create", status: 409,
			body: `{"message":"rpc error: code = AlreadyExists desc = name conflicts with an existing object"}`, conflict: true},
		{name: "create 400", run: create, route: "POST /v1.47/services/create", status: 400,
			body: `{"message":"invalid mount config"}`, bad: true},
		{name: "create 500", run: create, route: "POST /v1.47/services/create", status: 500,
			body: `{"message":"context deadline exceeded"}`, server: true, unknown: true},
		{name: "create 503", run: create, route: "POST /v1.47/services/create", status: 503,
			body: `{"message":"node is not part of a swarm"}`, server: true, unknown: true, unavailable: true},
		{name: "remove volume 409", run: rmVol, route: "DELETE /v1.47/volumes/v1", status: 409,
			body: `{"message":"remove v1: volume is in use - [aa86]"}`, conflict: true},
		{name: "remove volume 404", run: rmVol, route: "DELETE /v1.47/volumes/v1", status: 404,
			body: `{"message":"get v1: no such volume"}`, notFound: true},
		{name: "remove service 404 non-JSON", run: rmSvc, route: "DELETE /v1.47/services/svc1", status: 404,
			body: "not found\n", notFound: true, message: "not found"},
		{name: "remove service 500", run: rmSvc, route: "DELETE /v1.47/services/svc1", status: 500,
			body: `{"message":"boom"}`, server: true, unknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDaemon(t)
			f.route(tc.route, reply{status: tc.status, body: tc.body})
			err := tc.run(f.client(Options{}))
			if err == nil {
				t.Fatal("nil error")
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.StatusCode != tc.status {
				t.Fatalf("err = %v, want *APIError %d", err, tc.status)
			}
			if tc.message != "" && ae.Message != tc.message {
				t.Fatalf("message = %q, want %q", ae.Message, tc.message)
			}
			for _, m := range []struct {
				target error
				want   bool
			}{
				{agent.ErrObjectNotFound, tc.notFound}, {ErrConflict, tc.conflict}, {ErrServer, tc.server},
				{agent.ErrOutcomeUnknown, tc.unknown}, {ErrBadRequest, tc.bad}, {ErrUnavailable, tc.unavailable},
			} {
				if got := errors.Is(err, m.target); got != m.want {
					t.Errorf("errors.Is(%v) = %v, want %v (err: %v)", m.target, got, m.want, err)
				}
			}
		})
	}
}

func TestMalformedSuccessResponse(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{status: 201, body: `{"ID":`})
	f.route("GET /v1.47/services/svc1", reply{status: 200, body: `[`})
	c := f.client(Options{})
	if _, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i"}); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("create: %v, want ErrOutcomeUnknown", err)
	}
	f.route("POST /v1.47/services/create", reply{status: 201, body: `{}`})
	if _, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i"}); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("create without ID: %v, want ErrOutcomeUnknown", err)
	}
	if _, err := c.Inspect(context.Background(), agent.KindService, "svc1"); err == nil || errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("inspect: %v", err)
	}
}

func TestContextDeadlineAndCancel(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{block: true})
	f.route("GET /v1.47/services/svc1", reply{block: true})
	f.route("DELETE /v1.47/volumes/v1", reply{block: true})
	c := f.client(Options{})
	if _, err := c.NegotiateAPIVersion(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.CreateService(ctx, agent.ServiceSpec{Name: "s", Image: "i"})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("create: %v, want DeadlineExceeded and ErrOutcomeUnknown", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	_, err = c.Inspect(ctx2, agent.KindService, "svc1")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("inspect: %v, want DeadlineExceeded only", err)
	}

	ctx3, cancel3 := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel3() }()
	err = c.RemoveVolume(ctx3, "v1")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("remove volume: %v, want Canceled and ErrOutcomeUnknown", err)
	}

	// Already cancelled before sending: the dial never happens or fails;
	// the error must still carry context.Canceled.
	ctx4, cancel4 := context.WithCancel(context.Background())
	cancel4()
	if _, err := c.Inspect(ctx4, agent.KindService, "svc1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled inspect: %v", err)
	}
}

func TestDefaultTimeoutApplies(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/svc1", reply{block: true})
	c := f.client(Options{APIVersion: "1.47", Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Inspect(context.Background(), agent.KindService, "svc1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestDialFailureIsDefinite(t *testing.T) {
	dir, err := os.MkdirTemp("", "dc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := New(Options{Host: "unix://" + filepath.Join(dir, "missing.sock"), APIVersion: "1.47"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i"})
	if err == nil || errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want a definite dial error", err)
	}
}

func TestDroppedConnectionOnMutationIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(cannedVersion))
			return
		}
		// Hijack and close without a response: the request reached the
		// daemon, its effect is unknown.
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c, err := New(Options{Host: "tcp://" + srv.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RemoveService(context.Background(), "svc1"); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("remove: %v, want ErrOutcomeUnknown", err)
	}
	if _, err := c.Inspect(context.Background(), agent.KindService, "svc1"); err == nil || errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("inspect: %v", err)
	}
}

func TestTCPHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(cannedVersion))
		case "/v1.47/volumes/tardis":
			_, _ = w.Write([]byte(cannedVolume))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Options{Host: "tcp://" + srv.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v, err := c.Inspect(context.Background(), agent.KindVolume, "tardis"); err != nil || v.Name != "tardis" {
		t.Fatalf("inspect = %+v, %v", v, err)
	}
}
