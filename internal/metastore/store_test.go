// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

func mustApply(t *testing.T, n *Node, cmd Command) {
	t.Helper()
	if err := n.Apply(cmd); err != nil {
		t.Fatalf("Apply(%+v) on %s: %v", cmd, n.id, err)
	}
}

// waitForOp waits until op on every given node equals want.
func waitForOp(t *testing.T, nodes []*Node, opID string, want func(opjournal.Operation) bool) {
	t.Helper()
	for _, n := range nodes {
		waitFor(t, waitTimeout, "operation "+opID+" on "+n.id, func() bool {
			op, err := n.Journal().Get(opID)
			return err == nil && want(op)
		})
	}
}

func TestClusterElectsExactlyOneLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	if leader.Term() == 0 {
		t.Fatal("leader term is 0")
	}
	for _, n := range c.live(leader.id) {
		if n.IsLeader() {
			t.Fatalf("second leader %s next to %s", n.id, leader.id)
		}
	}
}

func TestApplyReplicatesToAllJournals(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	term := leader.Term()

	mustApply(t, leader, Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: []string{"a", "b"}, Term: term})
	mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: "a", Term: term})
	mustApply(t, leader, Command{Kind: "finish", OpID: "op1", StepID: "a", Result: opjournal.Succeeded, Term: term})

	want := opjournal.Operation{ID: "op1", Kind: "failover", Owner: term, Steps: []opjournal.Step{
		{ID: "a", State: opjournal.Succeeded, Attempts: 1},
		{ID: "b", State: opjournal.Pending},
	}}
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool { return reflect.DeepEqual(op, want) })
}

func TestApplyOnFollowerReturnsErrNotLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	f := c.follower(leader)
	err := f.Apply(Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: leader.Term()})
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Apply on follower: got %v, want ErrNotLeader", err)
	}
}

func TestApplyReturnsJournalError(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	term := leader.Term()
	mustApply(t, leader, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: term})

	if err := leader.Apply(Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: term}); !errors.Is(err, opjournal.ErrExists) {
		t.Fatalf("duplicate begin: got %v, want ErrExists", err)
	}
	if err := leader.Apply(Command{Kind: "start", OpID: "op1", StepID: "a", Term: term + 1}); !errors.Is(err, opjournal.ErrNotOwner) {
		t.Fatalf("start with foreign term: got %v, want ErrNotOwner", err)
	}
	if err := leader.Apply(Command{Kind: "bogus", OpID: "op1", Term: term}); err == nil {
		t.Fatal("unknown kind: got nil error")
	}
	// Rejected commands change nothing on any node.
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool {
		return op.Owner == term && op.Steps[0].State == opjournal.Pending
	})
}

func TestLeaderStopElectsNewLeaderKeepingState(t *testing.T) {
	c := newTestCluster(t, 3)
	old := c.waitForLeader()
	oldTerm := old.Term()
	mustApply(t, old, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: oldTerm})
	mustApply(t, old, Command{Kind: "start", OpID: "op1", StepID: "a", Term: oldTerm})

	c.stop(old.id)
	leader := c.waitForLeader()
	if leader.id == old.id {
		t.Fatal("stopped node is still leader")
	}
	if leader.Term() <= oldTerm {
		t.Fatalf("new term %d not greater than old term %d", leader.Term(), oldTerm)
	}
	// The new leader applies earlier-term entries once its own first entry
	// commits, so its local journal may briefly lag.
	waitForOp(t, []*Node{leader}, "op1", func(op opjournal.Operation) bool {
		return op.Owner == oldTerm && op.Steps[0].State == opjournal.Running
	})
	newTerm := leader.Term()
	mustApply(t, leader, Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool {
		return op.Owner == newTerm && op.Steps[0].State == opjournal.Unknown
	})
}

func TestPartitionedLeaderIsFencedAfterHeal(t *testing.T) {
	c := newTestCluster(t, 3)
	old := c.waitForLeader()
	oldTerm := old.Term()
	mustApply(t, old, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: oldTerm})
	waitForOp(t, c.live(), "op1", func(opjournal.Operation) bool { return true })

	c.Isolate(old.id)
	leader := c.waitForLeader(old.id)
	waitFor(t, waitTimeout, "isolated leader to step down", func() bool { return !old.IsLeader() })
	if err := old.Apply(Command{Kind: "start", OpID: "op1", StepID: "a", Term: oldTerm}); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Apply on isolated former leader: got %v, want ErrNotLeader", err)
	}

	newTerm := leader.Term()
	mustApply(t, leader, Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	// A command still carrying the old term is fenced by the journal.
	if err := leader.Apply(Command{Kind: "start", OpID: "op1", StepID: "a", Term: oldTerm}); !errors.Is(err, opjournal.ErrNotOwner) {
		t.Fatalf("start with old term: got %v, want ErrNotOwner", err)
	}
	mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: "a", Term: newTerm})

	c.Heal()
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool {
		return op.Owner == newTerm && op.Steps[0].State == opjournal.Running && op.Steps[0].Attempts == 1
	})
}

type bufferSink struct {
	bytes.Buffer
	cancelled bool
}

func (s *bufferSink) ID() string    { return "test" }
func (s *bufferSink) Close() error  { return nil }
func (s *bufferSink) Cancel() error { s.cancelled = true; return nil }

func applyLog(t *testing.T, f *fsm, index uint64, cmd Command) error {
	t.Helper()
	data, err := encodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	res := f.Apply(&raft.Log{Index: index, Term: cmd.Term, Type: raft.LogCommand, Data: data})
	r, ok := res.(applyResult)
	if !ok {
		t.Fatalf("Apply returned %T", res)
	}
	return r.err
}

func TestFSMSnapshotRestoreReproducesJournal(t *testing.T) {
	src := newFSM()
	cmds := []Command{
		{Kind: "begin", OpID: "op1", OpKind: "k", Steps: []string{"a", "b"}, Term: 3},
		{Kind: "start", OpID: "op1", StepID: "a", Term: 3},
		{Kind: "start", OpID: "op1", StepID: "b", Term: 3}, // rejected: a not succeeded
		{Kind: "begin", OpID: "op2", Steps: []string{"x"}, Term: 3},
		{Kind: "abort", OpID: "op2", Term: 3},
		{Kind: "adopt", OpID: "op1", Term: 5},
	}
	for i, cmd := range cmds {
		_ = applyLog(t, src, uint64(i+1), cmd)
	}
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &bufferSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()

	dst := newFSM()
	if err := dst.Restore(nopCloser{&sink.Buffer}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"op1", "op2"} {
		a, errA := src.journal().Get(id)
		b, errB := dst.journal().Get(id)
		if errA != nil || errB != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: src %+v (%v), restored %+v (%v)", id, a, errA, b, errB)
		}
	}
	if !reflect.DeepEqual(src.journal().Unfinished(), dst.journal().Unfinished()) {
		t.Fatal("Unfinished differs after restore")
	}
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }
