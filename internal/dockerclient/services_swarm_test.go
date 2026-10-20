// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// cannedSecret2 is abridged like cannedSecret.
const cannedSecret2 = `{"ID":"9fwdb4msjg9z5v6kebk3kxo01","Version":{"Index":12},"Spec":{"Name":"tls-key","Labels":{"dbcm.installation":"inst-1"}}}`

// Field names checked against the Docker Engine API reference v1.47:
// TaskSpec.ContainerSpec.Hostname (string), .StopGracePeriod (int64 ns),
// .Secrets[] {File {Name, UID, GID, Mode}, SecretID, SecretName},
// TaskSpec.Networks[] (NetworkAttachmentConfig {Target}) and
// ServiceSpec.EndpointSpec.Mode ("vip" or "dnsrr").
func TestCreateServiceSwarmFields(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/secrets/ktnbjxoalbkvbvedmg1urrz8h", reply{status: 200, body: cannedSecret})
	f.route("GET /v1.47/secrets/tls-key", reply{status: 200, body: cannedSecret2})
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	c := f.client(Options{})
	spec := agent.ServiceSpec{
		Name:              "dbcm-prod-n1",
		Image:             "mariadb:11.4",
		Replicas:          1,
		Hostname:          "dbcm-prod-n1",
		Networks:          []string{"7d86d31b1478e7cca9ebed7e73aa0fdeec46c5ca29497431d3007d2d9e15ed99"},
		EndpointMode:      agent.EndpointModeDNSRR,
		Secrets:           []string{"ktnbjxoalbkvbvedmg1urrz8h", "tls-key"},
		StopGracePeriodNS: 60_000_000_000,
	}
	if _, err := c.CreateService(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Path != "/v1.47/services/create" {
		t.Fatalf("last request %+v", r)
	}
	jsonEqual(t, r.Body, `{
		"Name": "dbcm-prod-n1",
		"TaskTemplate": {
			"ContainerSpec": {
				"Image": "mariadb:11.4",
				"Hostname": "dbcm-prod-n1",
				"StopGracePeriod": 60000000000,
				"Secrets": [
					{"File": {"Name": "app-dev.crt", "UID": "0", "GID": "0", "Mode": 292},
					 "SecretID": "ktnbjxoalbkvbvedmg1urrz8h", "SecretName": "app-dev.crt"},
					{"File": {"Name": "tls-key", "UID": "0", "GID": "0", "Mode": 292},
					 "SecretID": "9fwdb4msjg9z5v6kebk3kxo01", "SecretName": "tls-key"}
				]
			},
			"Networks": [{"Target": "7d86d31b1478e7cca9ebed7e73aa0fdeec46c5ca29497431d3007d2d9e15ed99"}]
		},
		"Mode": {"Replicated": {"Replicas": 1}},
		"EndpointSpec": {"Mode": "dnsrr"}
	}`)
}

func TestCreateServiceMissingSecretCreatesNothing(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	c := f.client(Options{})
	_, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i", Secrets: []string{"gone"}})
	if !errors.Is(err, agent.ErrObjectNotFound) {
		t.Fatalf("err = %v, want ErrObjectNotFound", err)
	}
	for _, r := range f.requests() {
		if r.Method == "POST" {
			t.Fatalf("mutating request sent: %+v", r)
		}
	}
}

func TestCreateServiceDuplicateSecretRefused(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/secrets/app-dev.crt", reply{status: 200, body: cannedSecret})
	f.route("GET /v1.47/secrets/ktnbjxoalbkvbvedmg1urrz8h", reply{status: 200, body: cannedSecret})
	c := f.client(Options{})
	_, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i",
		Secrets: []string{"app-dev.crt", "ktnbjxoalbkvbvedmg1urrz8h"}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	for _, r := range f.requests() {
		if r.Method == "POST" {
			t.Fatalf("mutating request sent: %+v", r)
		}
	}
}

func TestCreateServiceSwarmFieldValidation(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{})
	bad := []agent.ServiceSpec{
		{Name: "x", Image: "x", EndpointMode: "ingress"},
		{Name: "x", Image: "x", StopGracePeriodNS: -1},
		{Name: "x", Image: "x", Networks: []string{""}},
		{Name: "x", Image: "x", Secrets: []string{"../secrets"}},
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
