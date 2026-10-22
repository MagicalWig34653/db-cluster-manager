// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func TestInventoryReportsUnknownForMissingOrStaleObservations(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := s.Store()
	put := func(coll, key string, v any) {
		t.Helper()
		if _, err := Put(st, coll, key, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	put(CollSwarms, "a", SwarmRecord{ID: "a", Name: "never-observed"})
	put(CollSwarms, "b", SwarmRecord{ID: "b", Name: "stale", Observed: &Observation{State: api.StateHealthy, At: now.Add(-time.Hour)}})
	put(CollSwarms, "c", SwarmRecord{ID: "c", Name: "fresh", Observed: &Observation{State: api.StateDegraded, At: now.Add(-time.Second)}})
	put(CollSwarms, "d", SwarmRecord{ID: "d", Name: "bogus", Observed: &Observation{State: "great", At: now}})
	put(CollClusters, "k", ClusterRecord{ID: "k", Swarm: "c", Name: "shop", Engine: "mariadb-galera",
		Nodes: []galera.Node{{Slot: "n1", HostID: "h1"}, {Slot: "n2", HostID: "h2"}, {Slot: "n3", HostID: "h3"}},
		Health: map[string]NodeObservation{
			"n1": {Health: galera.NodeHealth{State: galera.HealthHealthy}, ObservedAt: now},
			"n2": {Health: galera.NodeHealth{State: galera.HealthHealthy}, ObservedAt: now.Add(-time.Hour)},
		}})
	inv := Inventory{Store: st, Now: func() time.Time { return now }}
	sws, err := inv.Swarms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]api.State{"a": api.StateUnknown, "b": api.StateUnknown, "c": api.StateDegraded, "d": api.StateUnknown}
	for _, sw := range sws {
		if sw.State != want[sw.ID] {
			t.Errorf("swarm %s: %s, want %s", sw.ID, sw.State, want[sw.ID])
		}
	}
	cl, err := inv.Cluster(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if cl.State != api.StateUnknown || cl.Nodes[0].State != api.StateHealthy || cl.Nodes[1].State != api.StateUnknown || cl.Nodes[2].State != api.StateUnknown {
		t.Fatalf("cluster %+v", cl)
	}
	if _, err := inv.Cluster(context.Background(), "missing"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("missing cluster: %v", err)
	}
	all, _ := inv.Clusters(context.Background())
	if len(all) != 1 {
		t.Fatalf("clusters %+v", all)
	}
}

func TestOperationStateMapping(t *testing.T) {
	cases := map[runner.Status]api.OperationState{
		runner.StatusRunning: api.OperationRunning, runner.StatusSucceeded: api.OperationSucceeded,
		runner.StatusFailed: api.OperationFailed, runner.StatusAborted: api.OperationFailed,
		runner.StatusPaused: api.OperationPending, runner.StatusStopped: api.OperationUnknown,
	}
	for st, want := range cases {
		if got := operationState(OperationRecord{View: &runner.View{Status: st}}); got != want {
			t.Errorf("%s: %s, want %s", st, got, want)
		}
	}
	if operationState(OperationRecord{}) != api.OperationPending {
		t.Error("operation without view")
	}
}

func TestEventsNewestFirstAndBounded(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	for i := range 5 {
		s.recordEvent("", "", api.SeverityInfo, string(rune('a'+i)))
	}
	inv := Inventory{Store: s.Store(), MaxEvents: 3}
	evs, err := inv.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The leadership-start event comes first in time, so the newest three
	// are e, d, c.
	if len(evs) != 3 || evs[0].Message != "e" || evs[2].Message != "c" {
		t.Fatalf("events %+v", evs)
	}
}
