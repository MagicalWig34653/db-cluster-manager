// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
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

// TestOperationProgressWhilePolling: lab E4 could only see state and a
// stale reason while a gate polled. The operations list now carries the
// current step and its latest unmet precondition, refreshed while it polls.
func TestOperationProgressWhilePolling(t *testing.T) {
	env := newBackupEnv(t, true)
	env.ops.MaxRetries = 10000 // keep polling the unmet source gate
	ref, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	var p *api.OperationProgress
	waitFor(t, "progress with polls", func() bool {
		ops, err := Inventory{Store: env.svc.Store()}.Operations(context.Background())
		if err != nil {
			return false
		}
		for _, o := range ops {
			if o.ID == ref.ID && o.Progress != nil && o.Progress.Polls >= 2 {
				p = o.Progress
				return o.State == api.OperationRunning
			}
		}
		return false
	})
	if p.Step != "01-check-source" || p.StepsDone != 0 || p.StepsTotal < 4 || p.Command != "" || p.Since.IsZero() || p.UpdatedAt.IsZero() ||
		!strings.Contains(p.Waiting, "backup-source-eligible") {
		t.Fatalf("progress %+v", p)
	}
	if err := env.ops.Abort(adminSubject, ref.ID); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, env.svc, ref.ID, runner.StatusAborted)
}

func TestOperationProgressFromView(t *testing.T) {
	at := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	v := runner.View{Steps: []runner.StepView{
		{ID: "s1", State: opjournal.Succeeded, Attempts: 1},
		{ID: "s2", Target: "h1", Command: "remove-volume", State: opjournal.Failed, Attempts: 2},
		{ID: "s3", State: opjournal.Pending},
	}, Timeline: []runner.Event{
		{At: at, Type: runner.EventStart, StepID: "s2"},
		{At: at.Add(time.Second), Type: runner.EventRetry, StepID: "s2", Detail: "step failed without effect (volume-in-use); retry 1 after 1s"},
		{At: at.Add(2 * time.Second), Type: runner.EventResult, StepID: "s1"},
	}}
	p := operationProgress(v, at)
	if p.StepsDone != 1 || p.StepsTotal != 3 || p.Step != "s2" || p.Target != "h1" || p.Attempts != 2 || p.Polls != 1 ||
		!strings.Contains(p.Waiting, "volume-in-use") || !p.Since.Equal(at.Add(time.Second)) {
		t.Fatalf("%+v", p)
	}
	if operationProgress(runner.View{}, at) != nil {
		t.Fatal("progress without steps")
	}
}

// TestOperationProgressShowsLastStepError: lab E4 failcreate showed only
// "idempotent step failed; retry N" for a failing exec-sql. The progress
// carries the latest failed answer of the current step, redacted.
func TestOperationProgressShowsLastStepError(t *testing.T) {
	at := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	v := runner.View{Steps: []runner.StepView{
		{ID: "05-exec-sql-n1", Target: "w1", Command: "exec-sql", State: opjournal.Failed, Attempts: 3},
	}, Timeline: []runner.Event{
		{At: at, Type: runner.EventStart, StepID: "05-exec-sql-n1"},
		{At: at, Type: runner.EventResult, StepID: "05-exec-sql-n1", Detail: "failed (db-unavailable): connect password=hunter2 refused"},
		{At: at, Type: runner.EventStart, StepID: "05-exec-sql-n1"},
		{At: at, Type: runner.EventResult, StepID: "05-exec-sql-n1", Detail: "failed (db-not-running): database socket /v/run/mysqld.sock does not exist"},
		{At: at, Type: runner.EventRetry, StepID: "05-exec-sql-n1", Detail: "dependency not running: ...; retry 2 after 2s"},
	}}
	p := operationProgress(v, at)
	if p.LastError != "failed (db-not-running): database socket /v/run/mysqld.sock does not exist" {
		t.Fatalf("last error %q", p.LastError)
	}
	v.Timeline = v.Timeline[:2]
	if p := operationProgress(v, at); strings.Contains(p.LastError, "hunter2") || !strings.Contains(p.LastError, "db-unavailable") {
		t.Fatalf("not redacted: %q", p.LastError)
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
