// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// abridged Node objects (API v1.47 definition Node) with documented field
// names; the engine version is the one seen on the lab VMs.
const cannedNodeList = `[{"ID":"n1","Version":{"Index":10},"Spec":{"Labels":{"zone":"a"},"Role":"manager","Availability":"active"},` +
	`"Description":{"Hostname":"m1","Platform":{"Architecture":"aarch64","OS":"linux"},"Resources":{"NanoCPUs":2000000000,"MemoryBytes":2147483648},` +
	`"Engine":{"EngineVersion":"29.9.1"}},"Status":{"State":"ready","Addr":"192.168.5.10"},"ManagerStatus":{"Leader":true,"Reachability":"reachable"}},` +
	`{"ID":"n2","Version":{"Index":11},"Spec":{"Role":"worker","Availability":"drain"},` +
	`"Description":{"Hostname":"w1","Engine":{"EngineVersion":"28.0.0"}},"Status":{"State":"down"}}]`

const cannedNode = `{"ID":"n1","Version":{"Index":10},"Spec":{"Labels":{"zone":"a"},"Role":"manager","Availability":"active","Name":"x"}}`

func TestNodesParsesInventory(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/nodes", reply{status: 200, body: cannedNodeList})
	nodes, err := f.client(Options{}).Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes %+v", nodes)
	}
	n := nodes[0]
	if n.ID != "n1" || n.Hostname != "m1" || n.Role != "manager" || n.Availability != "active" || n.State != "ready" ||
		!n.Leader || n.EngineVersion != "29.9.1" || n.Arch != "aarch64" || n.NanoCPUs != 2e9 || n.MemoryBytes != 2<<30 || n.Labels["zone"] != "a" {
		t.Fatalf("node %+v", n)
	}
	if nodes[1].Leader || nodes[1].State != "down" || nodes[1].Availability != "drain" {
		t.Fatalf("node %+v", nodes[1])
	}
}

func TestSetNodeLabelPreservesSpec(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/nodes/n1", reply{status: 200, body: cannedNode})
	f.route("POST /v1.47/nodes/n1/update", reply{status: 200, body: ``})
	c := f.client(Options{})
	if err := c.SetNodeLabel(context.Background(), "n1", "dbcm.agent.inst-1", "true"); err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Query != "version=10" {
		t.Fatalf("query %q", r.Query)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(r.Body), &spec); err != nil {
		t.Fatal(err)
	}
	labels := spec["Labels"].(map[string]any)
	if labels["zone"] != "a" || labels["dbcm.agent.inst-1"] != "true" || spec["Name"] != "x" || spec["Role"] != "manager" {
		t.Fatalf("posted spec %v", spec)
	}
	if err := c.SetNodeLabel(context.Background(), "n1", "zone", "b"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("foreign label: %v", err)
	}
}

func TestCreateGlobalService(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/services/create", reply{status: 201, body: cannedServiceCreate})
	id, err := f.client(Options{}).CreateGlobalService(context.Background(), agent.ServiceSpec{
		Name: "dbcm-agent", Image: "agent:1", Constraints: []string{"node.labels.dbcm.agent.inst-1==true"},
	})
	if err != nil || id == "" {
		t.Fatalf("id %q err %v", id, err)
	}
	body := f.last().Body
	if !strings.Contains(body, `"Mode":{"Global":{}}`) || strings.Contains(body, "Replicated") {
		t.Fatalf("body %s", body)
	}
}

func TestNodesManagerAddressFallback(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("GET /v1.47/nodes", reply{status: 200, body: `[{"ID":"m","Spec":{"Role":"manager"},"Status":{"State":"ready","Addr":"0.0.0.0"},` +
		`"ManagerStatus":{"Leader":true,"Addr":"10.0.0.4:2377"}},{"ID":"w","Spec":{"Role":"worker"},"Status":{"State":"ready","Addr":"10.0.0.5"}}]`})
	nodes, err := f.client(Options{}).Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].Addr != "10.0.0.4" || nodes[1].Addr != "10.0.0.5" {
		t.Fatalf("addresses %q %q", nodes[0].Addr, nodes[1].Addr)
	}
}

func TestCreateSecret(t *testing.T) {
	f := newFakeDaemon(t)
	f.route("POST /v1.47/secrets/create", reply{status: 201, body: `{"ID":"sec-1"}`})
	c := f.client(Options{})
	id, err := c.CreateSecret(context.Background(), "dbcm-agent-token-x", map[string]string{"dbcm.installation": "i"}, []byte("tok"))
	if err != nil || id != "sec-1" {
		t.Fatalf("id %q err %v", id, err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.last().Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["Name"] != "dbcm-agent-token-x" || body["Data"] != "dG9r" || body["Labels"].(map[string]any)["dbcm.installation"] != "i" {
		t.Fatalf("body %v", body)
	}
	for _, bad := range []struct {
		name string
		data []byte
	}{{"other", []byte("x")}, {"dbcm-x", nil}} {
		if _, err := c.CreateSecret(context.Background(), bad.name, nil, bad.data); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q: %v", bad.name, err)
		}
	}
}
