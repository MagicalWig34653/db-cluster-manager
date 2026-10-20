// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// metaCluster is an in-process three-node metastore cluster on raft's
// in-memory transport (no network). The metastore test helpers are not
// exported, so this is a minimal equivalent.
type metaCluster struct {
	t      *testing.T
	ids    []string
	trans  map[string]*raft.InmemTransport
	nodes  map[string]*metastore.Node
	closed map[string]bool
}

func newMetaCluster(t *testing.T) *metaCluster {
	t.Helper()
	c := &metaCluster{t: t, trans: map[string]*raft.InmemTransport{}, nodes: map[string]*metastore.Node{}, closed: map[string]bool{}}
	var peers []metastore.Peer
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("m%d", i)
		_, tr := raft.NewInmemTransport(raft.ServerAddress(id))
		c.ids = append(c.ids, id)
		c.trans[id] = tr
		peers = append(peers, metastore.Peer{ID: id, Address: id})
	}
	for _, a := range c.ids {
		for _, b := range c.ids {
			if a != b {
				c.trans[a].Connect(raft.ServerAddress(b), c.trans[b])
			}
		}
	}
	for _, id := range c.ids {
		n, err := metastore.NewNode(metastore.Config{
			ID: id, Transport: c.trans[id], Peers: peers, Bootstrap: true,
			HeartbeatTimeout: 50 * time.Millisecond, ElectionTimeout: 50 * time.Millisecond,
			LeaderLeaseTimeout: 50 * time.Millisecond, CommitTimeout: 5 * time.Millisecond,
			ApplyTimeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		c.nodes[id] = n
	}
	t.Cleanup(func() {
		for _, id := range c.ids {
			if !c.closed[id] {
				_ = c.nodes[id].Close()
			}
		}
	})
	return c
}

// leader waits for exactly one live leader other than exclude.
func (c *metaCluster) leader(exclude string) *metastore.Node {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var found []*metastore.Node
		for _, id := range c.ids {
			if id != exclude && !c.closed[id] && c.nodes[id].IsLeader() {
				found = append(found, c.nodes[id])
			}
		}
		if len(found) == 1 {
			return found[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatal("no leader elected")
	return nil
}

// kill disconnects and stops a node.
func (c *metaCluster) kill(id string) {
	for _, other := range c.ids {
		if other != id {
			c.trans[other].Disconnect(raft.ServerAddress(id))
		}
	}
	c.trans[id].DisconnectAll()
	_ = c.nodes[id].Close()
	c.closed[id] = true
}

func metaRunner(t *testing.T, n *metastore.Node, agents fakeAgents, checker Checker) *Runner {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := &fakeSleeper{}
	r, err := New(Config{Leader: n, Journal: MetastoreJournal{Node: n}, Agents: agents, Checker: checker,
		Now: clock.Now, Sleep: s.Sleep, BarrierTimeout: 2 * time.Second, StepTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestCreatePlanEndToEndOnMetastoreCluster runs a 3-node Galera create plan
// against fake agents with the journal replicated by a real in-process
// metastore cluster. The leader is stopped while a step's answer is lost; the
// new leader adopts, learns the recorded outcome and completes the plan.
// Every agent effect happens exactly once and the old term is fenced.
func TestCreatePlanEndToEndOnMetastoreCluster(t *testing.T) {
	c := newMetaCluster(t)
	agents := fakeAgents{"h1": newFakeAgent(t, "h1"), "h2": newFakeAgent(t, "h2"), "h3": newFakeAgent(t, "h3")}
	world := galeraWorld{agents: agents, cluster: "shop"}
	p := createPlan(t)
	op, err := FromGaleraPlan(p, createBindings("op-create"))
	if err != nil {
		t.Fatal(err)
	}

	first := c.leader("")
	oldTerm := first.Term()
	joiner := p.Nodes[1].HostID
	agents[joiner].addFault("07-create-service-n2", dropResponse)
	var once sync.Once
	agents[joiner].before = func(cmd agent.Command) {
		if cmd.StepID == "07-create-service-n2" {
			once.Do(func() { c.kill(first.ID()) })
		}
	}
	v1, err := metaRunner(t, first, agents, world).Run(context.Background(), op)
	if !errors.Is(err, ErrLostLeadership) || v1.Status != StatusStopped {
		t.Fatalf("first leader: err %v status %s", err, v1.Status)
	}

	second := c.leader(first.ID())
	if second.Term() <= oldTerm {
		t.Fatalf("new term %d not above %d", second.Term(), oldTerm)
	}
	r2 := metaRunner(t, second, agents, world)
	ids, err := r2.TakeOver(func(id string) (Operation, bool) { return op, id == op.ID })
	if err != nil || len(ids) != 1 {
		t.Fatalf("takeover %v %v", ids, err)
	}
	v2, err := r2.Resume(context.Background(), op.ID)
	if err != nil || v2.Status != StatusSucceeded {
		t.Fatalf("second leader: err %v status %s (%s)", err, v2.Status, v2.Reason)
	}
	effects := countEffects(agents)
	if len(effects) != 10 {
		t.Errorf("%d distinct effects, want 10: %v", len(effects), effects)
	}
	for e, n := range effects {
		if n != 1 {
			t.Errorf("effect %s executed %d times", e, n)
		}
	}
	// The lost-answer step was queried under the same IDs by the new term.
	var queried bool
	for _, cmd := range agents[joiner].Received() {
		if cmd.StepID == "07-create-service-n2" && cmd.Term == second.Term() {
			queried = true
		}
	}
	if !queried {
		t.Error("new leader did not query the in-flight step")
	}
	// A late command of the old term is rejected by every agent it reaches.
	for name, a := range agents {
		_, err := a.Handle(context.Background(), agent.Command{Term: oldTerm, OpID: "late", StepID: "x", Kind: agent.KindStopDB, Args: svcArgs("s")})
		if err == nil {
			t.Errorf("%s admitted stale term %d", name, oldTerm)
		}
	}
	// Replicas agree: the journal on the surviving follower shows success.
	for _, id := range c.ids {
		if c.closed[id] || id == second.ID() {
			continue
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if u := c.nodes[id].Journal().Unfinished(); len(u) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("follower %s still has unfinished operations", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
