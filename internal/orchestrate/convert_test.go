// SPDX-License-Identifier: AGPL-3.0-only

package orchestrate

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

func testCluster() galera.Cluster {
	return galera.Cluster{
		InstallationID:     "inst-1",
		ClusterID:          "clu-1",
		Name:               "shop",
		Image:              "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873",
		Network:            "dbcm-shop-galera",
		RootPasswordSecret: "dbcm-shop-root-pw",
		Members:            []string{"n1", "n2", "n3"},
		TLS:                galera.TLSOptions{Secrets: []string{"dbcm-shop-tls-key"}},
	}
}

func testNode(slot string) galera.Node {
	return galera.Node{
		Slot:     slot,
		NodeUUID: "81def889-15f7-44a3-b9db-3d4196e4b5b3",
		HostID:   "z4zp15cc49dfljfv2rx9q1kpf",
		Volume:   "dbcm-shop-" + slot + "-81def889",
		Resources: galera.Resources{
			Limits:       galera.ResourceAmount{MilliCPU: 2000, MemoryBytes: 2 << 30},
			Reservations: galera.ResourceAmount{MilliCPU: 500, MemoryBytes: 1 << 30},
		},
	}
}

func TestMilliToNanoCPUs(t *testing.T) {
	cases := []struct{ milli, nano int64 }{
		{1000, 1_000_000_000}, // 1 CPU
		{1, 1_000_000},
		{250, 250_000_000},
		{500, 500_000_000},
		{2500, 2_500_000_000},
		{16_000, 16_000_000_000},
		{0, 0},
		{-1, 0},
		{math.MaxInt64 / NanoCPUsPerMilliCPU, (math.MaxInt64 / NanoCPUsPerMilliCPU) * NanoCPUsPerMilliCPU},
		{math.MaxInt64/NanoCPUsPerMilliCPU + 1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64},
	}
	for _, c := range cases {
		if got := MilliToNanoCPUs(c.milli); got != c.nano {
			t.Errorf("MilliToNanoCPUs(%d) = %d, want %d", c.milli, got, c.nano)
		}
	}
}

func TestToAgentSpecFromBuildServiceSpec(t *testing.T) {
	g, err := galera.BuildServiceSpec(testCluster(), testNode("n2"), galera.SpecOptions{SSTDonor: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	a := ToAgentSpec(g)
	want := agent.ServiceSpec{
		Name:        "dbcm-shop-n2",
		Image:       g.Image,
		Labels:      g.Labels,
		Env:         g.Env,
		Args:        g.Args,
		Mounts:      []agent.Mount{{Type: "volume", Source: "dbcm-shop-n2-81def889", Target: galera.VolumeTarget}},
		Constraints: []string{"node.id==z4zp15cc49dfljfv2rx9q1kpf"},
		Replicas:    1,
		RestartPolicy: &agent.RestartPolicy{
			Condition: "any",
		},
		Resources: &agent.Resources{
			Limits:       &agent.ResourceLimits{NanoCPUs: 2_000_000_000, MemoryBytes: 2 << 30},
			Reservations: &agent.ResourceReservations{NanoCPUs: 500_000_000, MemoryBytes: 1 << 30},
		},
		Hostname:          "dbcm-shop-n2",
		Networks:          []string{"dbcm-shop-galera"},
		EndpointMode:      agent.EndpointModeDNSRR,
		Secrets:           []string{"dbcm-shop-root-pw", "dbcm-shop-tls-key"},
		StopGracePeriodNS: int64(60 * time.Second),
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("converted spec\n got %+v\nwant %+v", a, want)
	}
}

func TestToAgentSpecOmitsUnsetOptionalBlocks(t *testing.T) {
	a := ToAgentSpec(galera.ServiceSpec{Name: "s", Image: "i"})
	if a.RestartPolicy != nil || a.Resources != nil {
		t.Fatalf("unset restart policy/resources produced blocks: %+v", a)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	// Same encoding as a hand-written minimal agent spec.
	if string(b) != `{"name":"s","image":"i","replicas":0}` {
		t.Fatalf("encoding %s", b)
	}
	// Only a limit set: no reservations block, and vice versa.
	a = ToAgentSpec(galera.ServiceSpec{Resources: galera.Resources{Limits: galera.ResourceAmount{MemoryBytes: 1}}})
	if a.Resources == nil || a.Resources.Limits == nil || a.Resources.Reservations != nil {
		t.Fatalf("limits only: %+v", a.Resources)
	}
	a = ToAgentSpec(galera.ServiceSpec{Resources: galera.Resources{Reservations: galera.ResourceAmount{MilliCPU: 1}}})
	if a.Resources == nil || a.Resources.Limits != nil || a.Resources.Reservations.NanoCPUs != 1_000_000 {
		t.Fatalf("reservations only: %+v", a.Resources)
	}
}

func TestToAgentSpecSharesNothing(t *testing.T) {
	g, err := galera.BuildServiceSpec(testCluster(), testNode("n1"), galera.SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := g.WithoutBootstrap() // deep copy
	a := ToAgentSpec(g)
	a.Labels["x"] = "y"
	a.Env[0], a.Args[0], a.Constraints[0] = "x", "x", "x"
	a.Networks[0], a.Secrets[0] = "x", "x"
	a.Mounts[0].Source = "x"
	if !reflect.DeepEqual(g, before) {
		t.Fatal("mutating the converted spec changed the galera spec")
	}
	if !slices.Equal(g.Networks, []string{"dbcm-shop-galera"}) {
		t.Fatalf("networks %v", g.Networks)
	}
}
