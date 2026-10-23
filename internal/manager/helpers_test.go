// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const waitTimeout = 10 * time.Second

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fastRaft(c *metastore.Config) {
	c.HeartbeatTimeout = 50 * time.Millisecond
	c.ElectionTimeout = 50 * time.Millisecond
	c.LeaderLeaseTimeout = 50 * time.Millisecond
	c.CommitTimeout = 5 * time.Millisecond
	c.ApplyTimeout = 2 * time.Second
}

func fastRunner(c *runner.Config) {
	c.BackoffBase = 5 * time.Millisecond
	c.BackoffMax = 20 * time.Millisecond
	c.StepTimeout = 2 * time.Second
}

// cluster is an in-process group of Services on raft's in-memory transport.
type cluster struct {
	t      *testing.T
	ids    []string
	svcs   map[string]*Service
	closed map[string]bool
}

func newCluster(t *testing.T, size int, mut func(id string, c *Config)) *cluster {
	t.Helper()
	return newClusterWith(t, size, mut, nil)
}

// newClusterWith additionally calls post for each Service after New and
// before Start, so hooks can be registered without racing leadership.
func newClusterWith(t *testing.T, size int, mut func(id string, c *Config), post func(id string, s *Service)) *cluster {
	t.Helper()
	c := &cluster{t: t, svcs: map[string]*Service{}, closed: map[string]bool{}}
	trs := map[string]*raft.InmemTransport{}
	var peers []metastore.Peer
	for i := 1; i <= size; i++ {
		id := fmt.Sprintf("m%d", i)
		_, tr := raft.NewInmemTransport(raft.ServerAddress(id))
		trs[id] = tr
		c.ids = append(c.ids, id)
		peers = append(peers, metastore.Peer{ID: id, Address: id})
	}
	for _, a := range c.ids {
		for _, b := range c.ids {
			if a != b {
				trs[a].Connect(raft.ServerAddress(b), trs[b])
			}
		}
	}
	for _, id := range c.ids {
		cfg := Config{NodeID: id, Transport: trs[id], Bootstrap: true, Tune: fastRaft, RunnerTune: fastRunner,
			PollInterval: 10 * time.Millisecond, ViewSyncInterval: 20 * time.Millisecond}
		if size > 1 {
			cfg.Peers = peers
		}
		if mut != nil {
			mut(id, &cfg)
		}
		s, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		c.svcs[id] = s
		if post != nil {
			post(id, s)
		}
		s.Start()
	}
	t.Cleanup(func() {
		for _, id := range c.ids {
			if !c.closed[id] {
				_ = c.svcs[id].Close()
			}
		}
	})
	return c
}

func (c *cluster) live(exclude ...string) []*Service {
	skip := map[string]bool{}
	for _, id := range exclude {
		skip[id] = true
	}
	var out []*Service
	for _, id := range c.ids {
		if !c.closed[id] && !skip[id] {
			out = append(out, c.svcs[id])
		}
	}
	return out
}

// leader waits for exactly one live Service that completed its leadership
// start.
func (c *cluster) leader(exclude ...string) *Service {
	c.t.Helper()
	var l *Service
	waitFor(c.t, "a started leader", func() bool {
		l = nil
		n := 0
		for _, s := range c.live(exclude...) {
			if s.IsLeader() {
				l = s
				n++
			}
		}
		return n == 1
	})
	return l
}

func (c *cluster) follower(l *Service) *Service {
	for _, s := range c.live(l.cfg.NodeID) {
		return s
	}
	c.t.Fatal("no follower")
	return nil
}

func (c *cluster) close(s *Service) {
	c.t.Helper()
	if err := s.Close(); err != nil {
		c.t.Fatal(err)
	}
	c.closed[s.cfg.NodeID] = true
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

// sent is one command a fake agent received.
type sent struct {
	Target string
	Cmd    agent.Command
}

// fakeAgents records commands and answers like an agent: every step
// succeeds, create-service returns {"id": "svc-<step>"}, and a re-sent
// (OpID, StepID) returns the recorded result. block, if set, is called
// before answering and may hold the command until ctx ends.
type fakeAgents struct {
	mu      sync.Mutex
	sent    []sent
	results map[string]agent.Result
	block   func(ctx context.Context, cmd agent.Command) error
	// refuse, if set and true for a command, answers it as refused.
	refuse func(cmd agent.Command) bool
}

func newFakeAgents() *fakeAgents { return &fakeAgents{results: map[string]agent.Result{}} }

func (f *fakeAgents) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	f.mu.Lock()
	f.sent = append(f.sent, sent{Target: target, Cmd: cmd})
	key := cmd.OpID + "/" + cmd.StepID
	if r, ok := f.results[key]; ok {
		f.mu.Unlock()
		r.Replayed = true
		return r, nil
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		if err := block(ctx, cmd); err != nil {
			return agent.Result{}, err
		}
	}
	r := agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}
	if f.refuse != nil && f.refuse(cmd) {
		r.Status, r.Code, r.Message = agent.StatusRefused, agent.CodeNotOwned, "refused by the test"
	}
	if cmd.Kind == agent.KindCreateService {
		r.Output, _ = json.Marshal(map[string]string{"id": "svc-" + cmd.StepID})
	}
	f.mu.Lock()
	f.results[key] = r
	f.mu.Unlock()
	return r, nil
}

func (f *fakeAgents) commands() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

// okChecker accepts every precondition (the fake cluster is always synced).
type okChecker struct{}

func (okChecker) Check(context.Context, string, runner.Step, runner.Condition) (bool, string, error) {
	return true, "", nil
}

var testKey = StaticKey{ID: "k1", Bytes: []byte("0123456789abcdef0123456789abcdef")}

func mustPutRaw(t *testing.T, coll, key string, value []byte) metastore.DocOp {
	t.Helper()
	return metastore.DocOp{Collection: coll, Key: key, Value: value}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
