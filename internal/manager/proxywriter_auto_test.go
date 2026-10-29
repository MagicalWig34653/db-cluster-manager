// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

func TestProxyWriterAutomaticFailoverUsesFreshPrimaryHealthAndFreeze(t *testing.T) {
	group := newCluster(t, 3, nil)
	leader := group.leader()
	l := leader.Current()
	if l == nil {
		t.Fatal("no leadership")
	}
	route := ProxyWriterRoute{Cluster: "c1", InitialWriter: "n1", Candidates: []string{"n2", "n1", "n3"}, Members: []string{"p1", "p2"}}
	auto := ProxyWriterReconciler{Routes: []ProxyWriterRoute{route}, MaxAge: 30 * time.Second}
	coord := ProxyWriterStore{Store: leader.Store(), Cluster: route.Cluster, InitialWriter: route.InitialWriter, Candidates: route.Candidates, Members: route.Members}
	now := time.Now()
	good := galera.NodeHealth{State: galera.HealthHealthy, ClusterStatus: "Primary", LocalState: "Synced", Ready: true, Connected: true, ClusterSize: 2}
	bad := galera.NodeHealth{State: galera.HealthUnhealthy}
	rec := ClusterRecord{ID: "c1", Nodes: []galera.Node{{Slot: "n1"}, {Slot: "n2"}, {Slot: "n3"}}, Health: map[string]NodeObservation{
		"n1": {Health: good, ObservedAt: now}, "n2": {Health: good, ObservedAt: now}, "n3": {Health: bad, ObservedAt: now},
	}}
	put := func() {
		t.Helper()
		if _, err := Put(leader.Store(), CollClusters, "c1", rec, nil); err != nil {
			t.Fatal(err)
		}
	}
	put()
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, err := coord.Current()
	if err != nil || choice.Writer != "n1" {
		t.Fatalf("healthy writer moved: %+v %v", choice, err)
	}
	rec.Health["n1"] = NodeObservation{Health: bad, ObservedAt: now}
	put()
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, err = coord.Current()
	if err != nil || choice.Writer != "" || choice.Pending != "n2" {
		t.Fatalf("auto freeze: %+v %v", choice, err)
	}
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "" {
		t.Fatalf("pending switch bypassed ACKs: %+v", choice)
	}
	if _, err := coord.Ack("p1", choice.Epoch); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "" {
		t.Fatalf("one ACK promoted writer: %+v", choice)
	}
	if _, err := coord.Ack("p2", choice.Epoch); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "n2" {
		t.Fatalf("all ACKs: %+v", choice)
	}
	// A recovered former writer does not trigger a switch back.
	rec.Health["n1"] = NodeObservation{Health: good, ObservedAt: now}
	put()
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "n2" {
		t.Fatalf("flapping old writer: %+v", choice)
	}
	// Stale and non-Primary candidates cannot be promoted automatically.
	rec.Health["n2"] = NodeObservation{Health: bad, ObservedAt: now}
	rec.Health["n1"] = NodeObservation{Health: good, ObservedAt: now.Add(-time.Minute)}
	put()
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "n2" {
		t.Fatalf("stale candidate promoted: %+v", choice)
	}
	nonPrimary := good
	nonPrimary.ClusterStatus = "non-Primary"
	rec.Health["n1"] = NodeObservation{Health: nonPrimary, ObservedAt: now}
	put()
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, _ = coord.Current()
	if choice.Writer != "n2" {
		t.Fatalf("non-Primary candidate promoted: %+v", choice)
	}
}

// The manager generates cluster record IDs, while the proxy writer route is
// configured before the cluster exists; the route must therefore also match
// the cluster name.
func TestProxyWriterAutomaticFailoverMatchesClusterNameOfGeneratedID(t *testing.T) {
	group := newCluster(t, 3, nil)
	leader := group.leader()
	l := leader.Current()
	if l == nil {
		t.Fatal("no leadership")
	}
	route := ProxyWriterRoute{Cluster: "e9db", InitialWriter: "n1", Candidates: []string{"n1", "n2", "n3"}, Members: []string{"p1"}}
	auto := ProxyWriterReconciler{Routes: []ProxyWriterRoute{route}, MaxAge: 30 * time.Second}
	coord := ProxyWriterStore{Store: leader.Store(), Cluster: route.Cluster, InitialWriter: route.InitialWriter, Candidates: route.Candidates, Members: route.Members}
	now := time.Now()
	good := galera.NodeHealth{State: galera.HealthHealthy, ClusterStatus: "Primary", LocalState: "Synced", Ready: true, Connected: true, ClusterSize: 2}
	bad := galera.NodeHealth{State: galera.HealthUnhealthy}
	rec := ClusterRecord{ID: "cl-7f3a", Name: "e9db", Nodes: []galera.Node{{Slot: "n1"}, {Slot: "n2"}, {Slot: "n3"}}, Health: map[string]NodeObservation{
		"n1": {Health: bad, ObservedAt: now}, "n2": {Health: good, ObservedAt: now}, "n3": {Health: good, ObservedAt: now},
	}}
	if _, err := Put(leader.Store(), CollClusters, rec.ID, rec, nil); err != nil {
		t.Fatal(err)
	}
	if err := auto.ReconcileOnce(l); err != nil {
		t.Fatal(err)
	}
	choice, err := coord.Current()
	if err != nil || choice.Pending != "n2" {
		t.Fatalf("route named after the cluster did not freeze a switch: %+v %v", choice, err)
	}
}
