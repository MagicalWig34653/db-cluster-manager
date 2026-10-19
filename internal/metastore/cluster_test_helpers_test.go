// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// partitionNet is a switchable partition matrix shared by all test transports.
// An RPC is dropped when its sender or its target is isolated, so isolation is
// symmetric.
type partitionNet struct {
	mu       sync.RWMutex
	isolated map[raft.ServerAddress]bool
}

func (p *partitionNet) Isolate(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isolated[raft.ServerAddress(nodeID)] = true
}

func (p *partitionNet) Heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isolated = make(map[raft.ServerAddress]bool)
}

func (p *partitionNet) check(from, to raft.ServerAddress) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isolated[from] || p.isolated[to] {
		return fmt.Errorf("partitioned: %s -> %s", from, to)
	}
	return nil
}

// partTransport wraps raft's in-memory transport and drops RPCs across the
// partition matrix. Pipelining is disabled so every AppendEntries passes the
// check.
type partTransport struct {
	*raft.InmemTransport
	net *partitionNet
}

var (
	_ Transport        = (*partTransport)(nil)
	_ raft.WithPreVote = (*partTransport)(nil)
	_ raft.WithClose   = (*partTransport)(nil)
)

func (t *partTransport) AppendEntriesPipeline(raft.ServerID, raft.ServerAddress) (raft.AppendPipeline, error) {
	return nil, raft.ErrPipelineReplicationNotSupported
}

func (t *partTransport) AppendEntries(id raft.ServerID, target raft.ServerAddress, args *raft.AppendEntriesRequest, resp *raft.AppendEntriesResponse) error {
	if err := t.net.check(t.LocalAddr(), target); err != nil {
		return err
	}
	return t.InmemTransport.AppendEntries(id, target, args, resp)
}

func (t *partTransport) RequestVote(id raft.ServerID, target raft.ServerAddress, args *raft.RequestVoteRequest, resp *raft.RequestVoteResponse) error {
	if err := t.net.check(t.LocalAddr(), target); err != nil {
		return err
	}
	return t.InmemTransport.RequestVote(id, target, args, resp)
}

func (t *partTransport) RequestPreVote(id raft.ServerID, target raft.ServerAddress, args *raft.RequestPreVoteRequest, resp *raft.RequestPreVoteResponse) error {
	if err := t.net.check(t.LocalAddr(), target); err != nil {
		return err
	}
	return t.InmemTransport.RequestPreVote(id, target, args, resp)
}

func (t *partTransport) InstallSnapshot(id raft.ServerID, target raft.ServerAddress, args *raft.InstallSnapshotRequest, resp *raft.InstallSnapshotResponse, data io.Reader) error {
	if err := t.net.check(t.LocalAddr(), target); err != nil {
		return err
	}
	return t.InmemTransport.InstallSnapshot(id, target, args, resp, data)
}

func (t *partTransport) TimeoutNow(id raft.ServerID, target raft.ServerAddress, args *raft.TimeoutNowRequest, resp *raft.TimeoutNowResponse) error {
	if err := t.net.check(t.LocalAddr(), target); err != nil {
		return err
	}
	return t.InmemTransport.TimeoutNow(id, target, args, resp)
}

// testCluster is an in-process cluster of Nodes.
type testCluster struct {
	t       *testing.T
	net     *partitionNet
	ids     []string
	nodes   map[string]*Node
	stopped map[string]bool
}

func newTestCluster(t *testing.T, size int) *testCluster {
	t.Helper()
	c := &testCluster{
		t:       t,
		net:     &partitionNet{isolated: make(map[raft.ServerAddress]bool)},
		nodes:   make(map[string]*Node),
		stopped: make(map[string]bool),
	}
	inmem := make(map[string]*raft.InmemTransport)
	var peers []Peer
	for i := 1; i <= size; i++ {
		id := fmt.Sprintf("node%d", i)
		_, tr := raft.NewInmemTransport(raft.ServerAddress(id))
		inmem[id] = tr
		c.ids = append(c.ids, id)
		peers = append(peers, Peer{ID: id, Address: id})
	}
	for _, a := range c.ids {
		for _, b := range c.ids {
			if a != b {
				inmem[a].Connect(raft.ServerAddress(b), inmem[b])
			}
		}
	}
	for _, id := range c.ids {
		n, err := NewNode(Config{
			ID:                 id,
			Transport:          &partTransport{InmemTransport: inmem[id], net: c.net},
			Peers:              peers,
			Bootstrap:          true,
			HeartbeatTimeout:   50 * time.Millisecond,
			ElectionTimeout:    50 * time.Millisecond,
			LeaderLeaseTimeout: 50 * time.Millisecond,
			CommitTimeout:      5 * time.Millisecond,
			ApplyTimeout:       2 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewNode(%s): %v", id, err)
		}
		c.nodes[id] = n
	}
	t.Cleanup(func() {
		for _, id := range c.ids {
			if !c.stopped[id] {
				_ = c.nodes[id].Close()
			}
		}
	})
	return c
}

func (c *testCluster) Isolate(nodeID string) { c.net.Isolate(nodeID) }
func (c *testCluster) Heal()                 { c.net.Heal() }

// stop closes a node and isolates it so peers do not wait on its transport.
func (c *testCluster) stop(nodeID string) {
	c.t.Helper()
	c.net.Isolate(nodeID)
	if err := c.nodes[nodeID].Close(); err != nil {
		c.t.Fatalf("Close(%s): %v", nodeID, err)
	}
	c.stopped[nodeID] = true
}

// live returns the running nodes, excluding the given IDs.
func (c *testCluster) live(exclude ...string) []*Node {
	skip := make(map[string]bool)
	for _, id := range exclude {
		skip[id] = true
	}
	var out []*Node
	for _, id := range c.ids {
		if !c.stopped[id] && !skip[id] {
			out = append(out, c.nodes[id])
		}
	}
	return out
}

const waitTimeout = 10 * time.Second

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForLeader waits until exactly one live node outside exclude is leader
// and returns it.
func (c *testCluster) waitForLeader(exclude ...string) *Node {
	c.t.Helper()
	var leader *Node
	waitFor(c.t, waitTimeout, "exactly one leader", func() bool {
		leader = nil
		count := 0
		for _, n := range c.live(exclude...) {
			if n.IsLeader() {
				leader = n
				count++
			}
		}
		return count == 1
	})
	return leader
}

// follower returns some live node other than leader.
func (c *testCluster) follower(leader *Node) *Node {
	c.t.Helper()
	for _, n := range c.live(leader.id) {
		return n
	}
	c.t.Fatal("no follower")
	return nil
}
