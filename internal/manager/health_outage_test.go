// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// TestHealthCollectorReportsFullOutage: lab E6 stopped every node; the
// manager kept quiet (correctly changed nothing) but raised no event or
// notification either. The step into "unavailable" now records a critical
// event and one notification, the way back an info one, and nothing else
// (no recovery operation is planned).
func TestHealthCollectorReportsFullOutage(t *testing.T) {
	ha := &healthAgents{status: map[string]map[string]string{
		"h1": wsrep("Synced", "Primary", "3"), "h2": wsrep("Synced", "Primary", "3"), "h3": wsrep("Synced", "Primary", "3"),
	}, dbDown: map[string]bool{}}
	s, _ := healthSetup(t, ha, nil)
	var mu sync.Mutex
	kinds := map[string]int{}
	var outage notify.Event
	hc := HealthCollector{Notify: func(_ context.Context, e notify.Event) bool {
		mu.Lock()
		defer mu.Unlock()
		kinds[e.Kind]++
		if e.Kind == KindClusterOutage {
			outage = e
		}
		return true
	}}
	collect := func() {
		t.Helper()
		if err := hc.CollectOnce(context.Background(), s.Current()); err != nil {
			t.Fatal(err)
		}
	}
	setDown := func(down bool) {
		ha.mu.Lock()
		for _, h := range []string{"h1", "h2", "h3"} {
			ha.dbDown[h] = down
		}
		ha.mu.Unlock()
	}
	clusterEvents := func() []string {
		recs, _ := List[EventRecord](s.Store(), CollEvents)
		var out []string
		for _, r := range recs {
			if strings.Contains(r.Value.Message, "Primary component") {
				out = append(out, string(r.Value.Severity)+": "+r.Value.Message)
			}
		}
		return out
	}
	collect()
	setDown(true)
	collect()
	collect() // still down: no repetition
	ev := clusterEvents()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "critical: cluster shop has no Primary component") {
		t.Fatalf("events %v", ev)
	}
	mu.Lock()
	if kinds[KindClusterOutage] != 1 || outage.Severity != notify.SeverityCritical || outage.Resource != "swarm/sw1/cluster/clu-1" ||
		kinds[KindNodeUnavailable] != 3 {
		t.Fatalf("notifications %v %+v", kinds, outage)
	}
	mu.Unlock()
	if ops, _ := List[OperationRecord](s.Store(), CollOperations); len(ops) != 0 {
		t.Fatalf("outage planned operations %+v", ops)
	}
	setDown(false)
	collect()
	if ev := clusterEvents(); len(ev) != 2 || !strings.HasPrefix(ev[1], "info: cluster shop has a Primary component again") {
		t.Fatalf("events %v", ev)
	}
	mu.Lock()
	defer mu.Unlock()
	if kinds[KindClusterOutageEnded] != 1 {
		t.Fatalf("notifications %v", kinds)
	}
}

func TestClusterOutageChange(t *testing.T) {
	cases := []struct {
		prev, next api.State
		want       clusterOutageChange
	}{
		{"", api.StateUnavailable, outageNone},
		{api.StateHealthy, api.StateUnavailable, outageStarted},
		{api.StateDegraded, api.StateUnavailable, outageStarted},
		{api.StateUnknown, api.StateUnavailable, outageNone},
		{api.StateUnavailable, api.StateUnknown, outageNone},
		{api.StateUnavailable, api.StateDegraded, outageEnded},
		{api.StateUnavailable, api.StateHealthy, outageEnded},
		{api.StateHealthy, api.StateDegraded, outageNone},
	}
	for _, c := range cases {
		if got := clusterChange(c.prev, c.next); got != c.want {
			t.Errorf("%q -> %q: %v, want %v", c.prev, c.next, got, c.want)
		}
	}
}
