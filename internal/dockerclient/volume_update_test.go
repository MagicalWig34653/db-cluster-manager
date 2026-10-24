// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// cannedVolumeCreated is the 201 answer of POST /volumes/create (API v1.47
// Volume definition, abridged to documented fields) for a bind volume.
const cannedVolumeCreated = `{"Name":"shop-n1","Driver":"local","Mountpoint":"/var/lib/docker/volumes/shop-n1/_data",` +
	`"CreatedAt":"2026-01-02T03:04:05Z","Labels":{"dbcm.installation":"inst-1"},"Scope":"local",` +
	`"Options":{"device":"/srv/dbcm/shop-n1","o":"bind","type":"none"}}`

func TestCreateVolumeRequest(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/volumes/create", reply{status: 201, body: cannedVolumeCreated})
	c := f.client(Options{})
	err := c.CreateVolume(context.Background(), agent.VolumeSpec{Name: "shop-n1", Device: "/srv/dbcm/shop-n1",
		Labels: map[string]string{"dbcm.installation": "inst-1"}})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "POST" || r.Path != "/v1.47/volumes/create" || r.ContentType != "application/json" {
		t.Fatalf("request = %+v", r)
	}
	jsonEqual(t, r.Body, `{"Name":"shop-n1","Driver":"local","Labels":{"dbcm.installation":"inst-1"},`+
		`"DriverOpts":{"type":"none","o":"bind","device":"/srv/dbcm/shop-n1"}}`)
}

func TestCreateVolumeErrors(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/volumes/create", reply{status: 500, body: `{"message":"boom"}`})
	c := f.client(Options{})
	err := c.CreateVolume(context.Background(), agent.VolumeSpec{Name: "v", Device: "/srv/v"})
	if !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("500 err = %v, want outcome unknown", err)
	}
	f.route("POST /v1.47/volumes/create", reply{status: 201, body: cannedVolumeCreated})
	if err := c.CreateVolume(context.Background(), agent.VolumeSpec{Name: "other", Device: "/srv/v"}); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("mismatched name err = %v", err)
	}
	before := len(f.requests())
	for _, s := range []agent.VolumeSpec{{Name: "../x", Device: "/srv/x"}, {Name: "", Device: "/srv/x"}, {Name: "x", Device: "rel"}, {Name: "x"}} {
		if err := c.CreateVolume(context.Background(), s); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: err = %v", s, err)
		}
	}
	if len(f.requests()) != before {
		t.Fatal("request sent for invalid volume spec")
	}
}

func TestBindVolumeMountpointIsDevice(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/volumes/shop-n1", reply{status: 200, body: cannedVolumeCreated})
	f.route("GET /v1.47/volumes/tardis", reply{status: 200, body: cannedVolume})
	c := f.client(Options{})
	o, err := c.Inspect(context.Background(), agent.KindVolume, "shop-n1")
	if err != nil || o.Mountpoint != "/srv/dbcm/shop-n1" {
		t.Fatalf("bind volume: %+v, %v", o, err)
	}
	// A tmpfs volume keeps the daemon's mountpoint.
	o, err = c.Inspect(context.Background(), agent.KindVolume, "tardis")
	if err != nil || o.Mountpoint != "/var/lib/docker/volumes/tardis" {
		t.Fatalf("tmpfs volume: %+v, %v", o, err)
	}
}

func TestInspectServiceConstraintsAndReplicas(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/svc1", reply{status: 200, body: `{"ID":"svc1","Version":{"Index":4},"Spec":{"Name":"db","Labels":{"dbcm.installation":"i"},` +
		`"TaskTemplate":{"ContainerSpec":{"Image":"x"},"Placement":{"Constraints":["node.id==h1"]}},"Mode":{"Replicated":{"Replicas":1}}}}`})
	o, err := f.client(Options{}).Inspect(context.Background(), agent.KindService, "svc1")
	if err != nil || len(o.Constraints) != 1 || o.Constraints[0] != "node.id==h1" || o.Replicas != 1 {
		t.Fatalf("%+v, %v", o, err)
	}
}

func TestUpdateServiceMergesIntoCurrentSpec(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz", reply{status: 200, body: cannedService})
	f.route("POST /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz/update", reply{status: 200, body: `{"Warnings":null}`})
	c := f.client(Options{})
	err := c.UpdateService(context.Background(), "9mnpnzenvg8p8tdbtq4wvbkcz", agent.ServiceSpec{
		Name: "hopeful_cori", Image: "redis:7", Args: []string{"--appendonly", "yes"}, Replicas: 1,
		Labels:      map[string]string{"dbcm.installation": "inst-1"},
		Constraints: []string{"node.id==h1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "POST" || r.Query != "version=19" || r.ContentType != "application/json" {
		t.Fatalf("update request = %+v", r)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	// Unmodeled parts of the current spec survive.
	var full struct{ Spec map[string]json.RawMessage }
	_ = json.Unmarshal([]byte(cannedService), &full)
	for _, k := range []string{"UpdateConfig", "RollbackConfig", "EndpointSpec"} {
		jsonEqual(t, string(body[k]), string(full.Spec[k]))
	}
	jsonEqual(t, string(body["Mode"]), `{"Replicated":{"Replicas":1}}`)
	jsonEqual(t, string(body["Labels"]), `{"dbcm.installation":"inst-1"}`)
	var tt map[string]json.RawMessage
	_ = json.Unmarshal(body["TaskTemplate"], &tt)
	jsonEqual(t, string(tt["ForceUpdate"]), `0`)
	jsonEqual(t, string(tt["Placement"]), `{"Constraints":["node.id==h1"]}`)
	jsonEqual(t, string(tt["ContainerSpec"]), `{"Image":"redis:7","Args":["--appendonly","yes"],"Labels":{"dbcm.installation":"inst-1"}}`)
	// Modeled keys the new spec omits are dropped from the old one.
	if _, ok := tt["RestartPolicy"]; ok {
		t.Errorf("stale RestartPolicy kept: %s", tt["RestartPolicy"])
	}
	if _, ok := tt["Resources"]; ok {
		t.Errorf("stale Resources kept: %s", tt["Resources"])
	}
}

func TestUpdateServiceStaleVersionAndMissing(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/svc1", reply{status: 200, body: cannedOwnedService})
	f.route("POST /v1.47/services/svc1/update", reply{status: 500, body: `{"message":"update out of sequence"}`})
	c := f.client(Options{})
	s := agent.ServiceSpec{Name: "db-a", Image: "postgres:17"}
	if err := c.UpdateService(context.Background(), "svc1", s); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
	if err := c.UpdateService(context.Background(), "missing", s); !errors.Is(err, agent.ErrObjectNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	n := len(f.requests())
	if err := c.UpdateService(context.Background(), "../x", s); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad id: err = %v", err)
	}
	if err := c.UpdateService(context.Background(), "svc1", agent.ServiceSpec{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty spec: err = %v", err)
	}
	if len(f.requests()) != n {
		t.Fatal("request sent for invalid input")
	}
}

// An update that sets an update strategy replaces the service's
// UpdateConfig and RollbackConfig; one without keeps them (tested above).
func TestUpdateServiceReplacesUpdateConfig(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz", reply{status: 200, body: cannedService})
	f.route("POST /v1.47/services/9mnpnzenvg8p8tdbtq4wvbkcz/update", reply{status: 200, body: `{"Warnings":null}`})
	c := f.client(Options{})
	err := c.UpdateService(context.Background(), "9mnpnzenvg8p8tdbtq4wvbkcz", agent.ServiceSpec{
		Name: "hopeful_cori", Image: "redis:7.2", Replicas: 1,
		Labels:      map[string]string{"dbcm.installation": "inst-1"},
		Constraints: []string{"node.id==h1"},
		UpdateConfig: &agent.UpdateConfig{Parallelism: 1, DelayNS: 5e9, FailureAction: agent.FailureActionRollback,
			MonitorNS: 30e9, Order: agent.UpdateStartFirst},
		RollbackConfig: &agent.UpdateConfig{Parallelism: 1, FailureAction: agent.FailureActionPause, Order: agent.UpdateStartFirst},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.last().Body), &body); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, string(body["UpdateConfig"]), `{"Parallelism":1,"Delay":5000000000,"FailureAction":"rollback","Monitor":30000000000,"Order":"start-first"}`)
	jsonEqual(t, string(body["RollbackConfig"]), `{"Parallelism":1,"FailureAction":"pause","Order":"start-first"}`)
}

func TestUpdateConfigValidation(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{})
	bad := []agent.ServiceSpec{
		{Name: "a", Image: "x:1", UpdateConfig: &agent.UpdateConfig{Parallelism: 0}},
		{Name: "a", Image: "x:1", UpdateConfig: &agent.UpdateConfig{Parallelism: 1, Order: "sideways"}},
		{Name: "a", Image: "x:1", UpdateConfig: &agent.UpdateConfig{Parallelism: 1, MaxFailureRatio: 2}},
		{Name: "a", Image: "x:1", RollbackConfig: &agent.UpdateConfig{Parallelism: 1, FailureAction: agent.FailureActionRollback}},
	}
	for _, s := range bad {
		if _, err := c.CreateService(context.Background(), s); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: err = %v", s.UpdateConfig, err)
		}
	}
	if len(f.requests()) != 0 {
		t.Fatal("request sent for an invalid update config")
	}
}
