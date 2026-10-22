// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

type countingReconciler struct {
	running atomic.Int32
	starts  atomic.Int32
}

func (r *countingReconciler) Name() string { return "count" }
func (r *countingReconciler) Run(ctx context.Context, _ *Leadership) error {
	r.starts.Add(1)
	r.running.Add(1)
	defer r.running.Add(-1)
	<-ctx.Done()
	return nil
}

func TestOnlyOneReplicaLeadsAndReconcilersFollowLeadership(t *testing.T) {
	rec := &countingReconciler{}
	c := newCluster(t, 3, func(_ string, cfg *Config) { cfg.Reconcilers = []Reconciler{rec} })
	l := c.leader()
	waitFor(t, "reconciler running", func() bool { return rec.running.Load() == 1 })
	for _, s := range c.live(l.cfg.NodeID) {
		if s.IsLeader() || s.Current() != nil {
			t.Fatal("follower reports leadership")
		}
	}
	if id, _ := c.follower(l).LeaderAddress(); id != l.cfg.NodeID {
		t.Fatalf("follower knows leader %q, want %q", id, l.cfg.NodeID)
	}
	var lost atomic.Bool
	l.OnLeadership(func(x *Leadership) {
		if x == nil {
			lost.Store(true)
		}
	})
	c.close(l)
	if !lost.Load() {
		t.Fatal("loss hook not called")
	}
	c.leader(l.cfg.NodeID)
	waitFor(t, "reconciler restarted on the new leader", func() bool { return rec.starts.Load() == 2 && rec.running.Load() == 1 })
}

func TestTakeOverWithoutAutoResumeSendsNothing(t *testing.T) {
	agents := newFakeAgents()
	entered := make(chan struct{}, 1)
	agents.block = func(ctx context.Context, cmd agent.Command) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	c := newCluster(t, 3, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	old := c.leader()
	ops := seed(t, old, 3)
	rec, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	c.close(old)
	n := len(agents.commands())
	next := c.leader(old.cfg.NodeID)
	got := waitOpStatus(t, next, rec.ID, runner.StatusPaused)
	if len(next.Current().Adopted) != 1 || next.Current().Adopted[0] != rec.ID {
		t.Fatalf("adopted %v", next.Current().Adopted)
	}
	if len(agents.commands()) != n {
		t.Fatal("new leader sent commands without resume")
	}
	if got.View.Term != next.Current().Term {
		t.Fatalf("view term %d", got.View.Term)
	}
	// An explicit resume continues it.
	agents.mu.Lock()
	agents.block = nil
	agents.mu.Unlock()
	if err := opsFor(t, next).Resume(adminSubject, rec.ID); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, next, rec.ID, runner.StatusSucceeded)
}
