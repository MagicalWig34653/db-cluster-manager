// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// The galera service type must produce exactly the plans and operations of
// the direct cluster path and must not open a way around its safety rules
// (quorum, verified donor, recovery approval). Fakes only.

const gib = int64(1) << 30

func seqUUIDs(prefix string) func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("%s-0000-4000-8000-%012x", prefix, n), nil
	}
}

var galeraRes = galera.Resources{
	Limits:       galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib},
	Reservations: galera.ResourceAmount{MilliCPU: 500, MemoryBytes: gib},
}

const galeraImage = "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873"

func galeraSpec(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(service.GaleraSpec{Size: 3, Image: galeraImage, Network: "dbcm-shop-galera",
		RootPasswordSecret: "dbcm-shop-root-pw", Resources: galeraRes, Placement: galera.PlacementPolicy{Preferred: []string{"h2"}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func galeraHosts(ids ...string) []galera.Host {
	var out []galera.Host
	for _, id := range ids {
		out = append(out, galera.Host{ID: id, Ready: true, Active: true, Capacity: galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}})
	}
	return out
}

var accounts = galera.Accounts{MonitorSecret: "dbcm-monitor-pw", BackupSecret: "dbcm-backup-pw"}

// directCreate is the manager's direct cluster creation planning
// (Operations.CreateCluster) for the same request.
func directCreate(t *testing.T, hosts []galera.Host) galera.Plan {
	t.Helper()
	p, err := galera.PlanCreate(galera.Topology{
		Cluster: galera.Cluster{InstallationID: "inst-1", ClusterID: "clu-1", Name: "shop", Image: galeraImage,
			Network: "dbcm-shop-galera", RootPasswordSecret: "dbcm-shop-root-pw", Accounts: accounts},
		Size: 3, Resources: galeraRes, NewUUID: seqUUIDs("00000000"),
	}, hosts, galera.PlacementPolicy{Preferred: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func typeCreate(t *testing.T, hosts []galera.Host) galera.Plan {
	t.Helper()
	p, err := service.Galera{NewUUID: seqUUIDs("00000000")}.PlanCluster(service.GaleraInput{
		Context: service.Context{InstallationID: "inst-1", ServiceID: "clu-1", Name: "shop"},
		Spec:    galeraSpec(t), Hosts: hosts, Accounts: accounts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func galeraBindings(op string) runner.GaleraBindings {
	return runner.GaleraBindings{OpID: op, InstallationID: "inst-1", VolumeBase: "/var/lib/dbcm/volumes",
		TokenIDs: map[string]string{"n1": "t1", "n2": "t2", "n3": "t3"}, StepTimeout: time.Minute, MaxRetries: 3, ServiceHost: "mgr"}
}

func TestGaleraCreateIdenticalToDirectPath(t *testing.T) {
	hosts := galeraHosts("h1", "h2", "h3", "h4")
	direct, viaType := directCreate(t, hosts), typeCreate(t, hosts)
	if !reflect.DeepEqual(direct, viaType) {
		t.Fatalf("plans differ\ndirect %+v\ntype   %+v", direct, viaType)
	}
	opDirect, err := runner.FromGaleraPlan(direct, galeraBindings("op-1"))
	if err != nil {
		t.Fatal(err)
	}
	opType, err := runner.FromGaleraPlan(viaType, galeraBindings("op-1"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(opDirect)
	b, _ := json.Marshal(opType)
	if string(a) != string(b) {
		t.Fatalf("operations differ\ndirect %s\ntype   %s", a, b)
	}

	// The generic Type interface delegates to the same planner and attaches
	// the plan unchanged; its description lists the same steps.
	ty, err := service.DefaultRegistry().Get(service.TypeGalera)
	if err != nil {
		t.Fatal(err)
	}
	if _, isUpdater := ty.(service.Updater); isUpdater {
		t.Fatal("galera must not have the generic update strategy")
	}
	gen, err := service.Galera{NewUUID: seqUUIDs("00000000")}.Plan(service.PlanInput{
		Context: service.Context{InstallationID: "inst-1", ServiceID: "clu-1", Name: "shop"}, Spec: galeraSpec(t),
		Hosts: []service.Host{
			{ID: "h1", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
			{ID: "h2", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
			{ID: "h3", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
			{ID: "h4", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !gen.Delegated() || len(gen.Steps) != len(gen.Galera.Steps) {
		t.Fatalf("description: delegated %v, %d steps for %d", gen.Delegated(), len(gen.Steps), len(gen.Galera.Steps))
	}
	for i, s := range gen.Steps {
		gs := gen.Galera.Steps[i]
		if s.ID != gs.ID || string(s.Kind) != string(gs.Kind) || s.HostID != gs.HostID {
			t.Errorf("step %d described as %s/%s on %s, plan has %s/%s on %s", i, s.ID, s.Kind, s.HostID, gs.ID, gs.Kind, gs.HostID)
		}
	}
	if !strings.Contains(gen.Text(), "Synced and Primary at cluster size 1") {
		t.Errorf("the description hides the join gates:\n%s", gen.Text())
	}
	// Without accounts the generic path keeps the planner's own warning.
	if !strings.Contains(strings.Join(gen.Warnings, "\n"), "no monitoring account") {
		t.Errorf("warnings %v", gen.Warnings)
	}
}

// A delegated plan never becomes an operation through the generic service
// conversion, so none of FromGaleraPlan's refusals can be sidestepped.
func TestGaleraPlanRefusedByServiceConversion(t *testing.T) {
	gen, err := service.Galera{NewUUID: seqUUIDs("00000000")}.Plan(service.PlanInput{
		Context: service.Context{InstallationID: "inst-1", ServiceID: "clu-1", Name: "shop"}, Spec: galeraSpec(t),
		Hosts: []service.Host{
			{ID: "h1", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
			{ID: "h2", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
			{ID: "h3", Ready: true, Active: true, Capacity: service.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sb := runner.ServiceBindings{OpID: "op", InstallationID: "inst-1", VolumeBase: "/srv", ServiceHost: "mgr"}
	if _, err := runner.FromServicePlan(gen, sb); !errors.Is(err, runner.ErrInvalidOperation) {
		t.Fatalf("delegated plan converted: %v", err)
	}
	// Stripping the attached plan does not help: the type is refused too.
	stripped := gen
	stripped.Galera = nil
	if _, err := runner.FromServicePlan(stripped, sb); !errors.Is(err, runner.ErrInvalidOperation) {
		t.Fatalf("galera description converted: %v", err)
	}
}

const recoveryUUID = "3b2d2499-ce8f-11f1-a8b6-67c327274574"

func synced(size string) galera.NodeHealth {
	return galera.HealthFromStatus(map[string]string{
		"wsrep_cluster_size": size, "wsrep_cluster_status": "Primary", "wsrep_local_state_comment": "Synced",
		"wsrep_ready": "ON", "wsrep_connected": "ON", "wsrep_cluster_state_uuid": "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e",
		"wsrep_last_committed": "11", "wsrep_flow_control_paused": "0.000000", "wsrep_local_recv_queue_avg": "0.000000",
	})
}

func clusterState(t *testing.T, healthy ...string) (galera.ClusterState, time.Time) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	created := directCreate(t, galeraHosts("h1", "h2", "h3"))
	st := galera.ClusterState{Cluster: created.Cluster, LastKnownSize: 3, Hosts: galeraHosts("h1", "h2", "h3", "h4")}
	for _, n := range created.Nodes {
		h := galera.NodeHealth{}
		for _, s := range healthy {
			if s == n.Slot {
				h = synced(fmt.Sprint(len(healthy)))
			}
		}
		st.Nodes = append(st.Nodes, galera.NodeState{Node: n, Health: h, ObservedAt: now.Add(-5 * time.Second)})
	}
	return st, now
}

func TestGaleraReplacementKeepsQuorumRules(t *testing.T) {
	g := service.Galera{}
	// Quorum and a Synced donor: the same plan as the direct call.
	st, now := clusterState(t, "n1", "n2")
	pol := func() galera.ReplacementPolicy {
		return galera.ReplacementPolicy{Now: now, NewUUID: seqUUIDs("11111111")}
	}
	direct, err := galera.PlanReplacement(st, "n3", pol())
	if err != nil {
		t.Fatal(err)
	}
	viaType, err := g.PlanReplacement(st, "n3", pol())
	if err != nil || !reflect.DeepEqual(direct, viaType) {
		t.Fatalf("replacement differs: %v", err)
	}

	// One of three nodes left: no majority. The type refuses exactly like
	// the direct path; there is no override.
	lone, now := clusterState(t, "n1")
	_, errDirect := galera.PlanReplacement(lone, "n3", galera.ReplacementPolicy{Now: now})
	_, errType := g.PlanReplacement(lone, "n3", galera.ReplacementPolicy{Now: now})
	if !errors.Is(errType, galera.ErrRequiresGuidedRecovery) || errType.Error() != errDirect.Error() {
		t.Fatalf("no-quorum replacement: type %v, direct %v", errType, errDirect)
	}
	// Stale observations count as unknown, also through the type.
	_, err = g.PlanReplacement(st, "n3", galera.ReplacementPolicy{Now: now.Add(time.Hour)})
	if !errors.Is(err, galera.ErrRequiresGuidedRecovery) {
		t.Fatalf("stale observations: %v", err)
	}
}

func TestGaleraRecoveryKeepsApproval(t *testing.T) {
	created := directCreate(t, galeraHosts("h1", "h2", "h3"))
	gs := func(seqno int64) *galera.Grastate {
		return &galera.Grastate{Version: "2.1", UUID: recoveryUUID, Seqno: seqno, HasSafeToBootstrap: true}
	}
	d, err := galera.DiagnoseFullOutage([]galera.NodeInfo{
		{Slot: "n1", Grastate: gs(3517)}, {Slot: "n2", Grastate: gs(3518)}, {Slot: "n3", Grastate: gs(3516)},
	})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := galera.RecoveryPlan(d, created.Cluster, created.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	viaType, err := service.Galera{}.PlanRecovery(d, created.Cluster, created.Nodes)
	if err != nil || !reflect.DeepEqual(direct, viaType) {
		t.Fatalf("recovery differs: %v", err)
	}
	if !viaType.RequiresApproval {
		t.Fatal("recovery plan without approval requirement")
	}
	b := runner.GaleraBindings{OpID: "op", InstallationID: "inst-1", VolumeBase: "/var/lib/dbcm/volumes",
		ServiceIDs: map[string]string{"n1": "s1", "n2": "s2", "n3": "s3"}, ServiceHost: "mgr"}
	if _, err := runner.FromGaleraPlan(viaType, b); !errors.Is(err, runner.ErrInvalidOperation) {
		t.Fatalf("recovery without approval ID converted: %v", err)
	}
	desc := service.DescribeGaleraPlan(service.Context{Name: "shop"}, viaType)
	if !strings.Contains(desc.Text(), "approved by a user") {
		t.Errorf("description hides the approval:\n%s", desc.Text())
	}
	if _, err := runner.FromServicePlan(desc, runner.ServiceBindings{OpID: "op", InstallationID: "inst-1", VolumeBase: "/srv", ServiceHost: "mgr"}); !errors.Is(err, runner.ErrInvalidOperation) {
		t.Fatalf("recovery description converted without approval: %v", err)
	}
}

func TestGaleraUnsupportedRequests(t *testing.T) {
	g := service.Galera{}
	if _, err := g.PlanScale(5); !errors.Is(err, service.ErrGaleraScale) {
		t.Errorf("scale: %v", err)
	}
	if _, err := g.DeletePlan(service.PlanInput{}); !errors.Is(err, service.ErrDelegated) {
		t.Errorf("delete: %v", err)
	}
	if _, err := service.UpdatePlan(g, service.UpdateInput{}); !errors.Is(err, service.ErrUpdateUnsupported) {
		t.Errorf("update: %v", err)
	}
	for _, bad := range []string{
		`{"size":2,"image":"m:1","network":"n","rootPasswordSecret":"s","resources":{"limits":{},"reservations":{}},"placement":{"reserve":{}}}`,
		`{"size":3,"image":"","network":"n","rootPasswordSecret":"s","resources":{"limits":{},"reservations":{}},"placement":{"reserve":{}}}`,
		`{"size":3,"image":"m:1","network":"n","rootPasswordSecret":"s","resources":{"limits":{},"reservations":{}},"placement":{"reserve":{}},"bootstrap":true}`,
	} {
		if err := g.Validate(json.RawMessage(bad)); !errors.Is(err, service.ErrInvalidSpec) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// Maintenance delegates too.
	created := directCreate(t, galeraHosts("h1", "h2", "h3"))
	direct, errD := galera.MaintenancePlan(created.Cluster, created.Nodes, created.Nodes[0].HostID)
	viaType, errT := g.PlanMaintenance(created.Cluster, created.Nodes, created.Nodes[0].HostID, false)
	if (errD == nil) != (errT == nil) || !reflect.DeepEqual(direct, viaType) {
		t.Errorf("maintenance differs: %v / %v", errD, errT)
	}
}
