// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// cannedConfig is abridged like cannedSecret (Swarm Config: ID, Spec.Name).
const cannedConfig = `{"ID":"d3nc0nf1gid1234567890abcd","Version":{"Index":3},"Spec":{"Name":"sftp-users","Labels":{"dbcm.installation":"inst-1"}}}`

// Field names checked against the Docker Engine API reference v1.47:
// ServiceSpec.EndpointSpec.Ports[] {Protocol, TargetPort, PublishedPort,
// PublishMode ("ingress" or "host")} and
// TaskSpec.ContainerSpec.Configs[] {File {Name, UID, GID, Mode}, ConfigID,
// ConfigName}.
func TestCreateServicePortsAndConfigs(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/configs/d3nc0nf1gid1234567890abcd", reply{status: 200, body: cannedConfig})
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	c := f.client(Options{})
	spec := agent.ServiceSpec{
		Name: "dbcm-svc-files", Image: "example/sftp:1", Replicas: 1,
		Ports: []agent.PortSpec{
			{Target: 22, Published: 2222, Mode: agent.PortModeHost},
			{Target: 53, Published: 5353, Protocol: agent.PortProtocolUDP},
		},
		Configs: []agent.ConfigRef{{Config: "d3nc0nf1gid1234567890abcd", Target: "/etc/dbcm/sftp-users.json"}},
	}
	if _, err := c.CreateService(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, f.last().Body, `{
		"Name": "dbcm-svc-files",
		"TaskTemplate": {"ContainerSpec": {
			"Image": "example/sftp:1",
			"Configs": [{"File": {"Name": "/etc/dbcm/sftp-users.json", "UID": "0", "GID": "0", "Mode": 292},
			             "ConfigID": "d3nc0nf1gid1234567890abcd", "ConfigName": "sftp-users"}]
		}},
		"Mode": {"Replicated": {"Replicas": 1}},
		"EndpointSpec": {"Ports": [
			{"Protocol": "tcp", "TargetPort": 22, "PublishedPort": 2222, "PublishMode": "host"},
			{"Protocol": "udp", "TargetPort": 53, "PublishedPort": 5353, "PublishMode": "ingress"}
		]}
	}`)
}

func TestCreateServiceMissingConfigCreatesNothing(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{})
	_, err := c.CreateService(context.Background(), agent.ServiceSpec{Name: "s", Image: "i",
		Configs: []agent.ConfigRef{{Config: "gone", Target: "/x"}}})
	if err == nil {
		t.Fatal("missing config accepted")
	}
	for _, r := range f.requests() {
		if r.Method == "POST" {
			t.Fatalf("mutating request sent: %+v", r)
		}
	}
}

func TestCreateServicePortConfigValidation(t *testing.T) {
	f := newFakeDaemon(t)
	c := f.client(Options{})
	bad := []agent.ServiceSpec{
		{Name: "x", Image: "x", Ports: []agent.PortSpec{{Target: 80}}},
		{Name: "x", Image: "x", Ports: []agent.PortSpec{{Target: 80, Published: 99999}}},
		{Name: "x", Image: "x", Ports: []agent.PortSpec{{Target: 80, Published: 80, Protocol: "icmp"}}},
		{Name: "x", Image: "x", Ports: []agent.PortSpec{{Target: 80, Published: 80, Mode: "bridge"}}},
		{Name: "x", Image: "x", Configs: []agent.ConfigRef{{Config: "../c", Target: "/x"}}},
		{Name: "x", Image: "x", Configs: []agent.ConfigRef{{Config: "c", Target: "x"}}},
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
