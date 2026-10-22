// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// freeLoopbackAddr returns a loopback TCP address that was free a moment ago.
// The port is reused across node restarts so the stored Raft configuration
// stays valid.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// tcpCluster runs Nodes with the real TCP transport on loopback and disk
// storage in per-node temporary directories.
type tcpCluster struct {
	t     *testing.T
	ids   []string
	peers []Peer
	addrs map[string]string
	dirs  map[string]string
	nodes map[string]*Node // nil while stopped
}

func newTCPCluster(t *testing.T, size int) *tcpCluster {
	t.Helper()
	c := &tcpCluster{t: t, addrs: map[string]string{}, dirs: map[string]string{}, nodes: map[string]*Node{}}
	for i := 1; i <= size; i++ {
		id := fmt.Sprintf("node%d", i)
		c.ids = append(c.ids, id)
		c.addrs[id] = freeLoopbackAddr(t)
		c.dirs[id] = t.TempDir()
		c.peers = append(c.peers, Peer{ID: id, Address: c.addrs[id]})
	}
	t.Cleanup(func() {
		for _, id := range c.ids {
			if n := c.nodes[id]; n != nil {
				_ = n.Close()
			}
		}
	})
	for _, id := range c.ids {
		c.start(id)
	}
	return c
}

// start starts (or restarts) a node from its data directory. Bootstrap is
// always set: on a restart the node must skip it because state exists.
func (c *tcpCluster) start(id string) *Node {
	c.t.Helper()
	n, err := NewNode(Config{
		ID:        id,
		BindAddr:  c.addrs[id],
		DataDir:   c.dirs[id],
		Peers:     c.peers,
		Bootstrap: true,
		// Each node fsyncs its bbolt log on every append. With 100ms
		// timeouts, a race-enabled run on a loaded host missed heartbeats
		// and leases often enough that the leader stepped down in the middle
		// of a test (raft logs: "heartbeat timeout reached", "failed to
		// contact quorum of nodes, stepping down", repeated split votes).
		// These timeouts stay well below the library defaults (1s/1s/500ms)
		// and tests additionally tolerate leader changes (applyOnLeader).
		HeartbeatTimeout:   tcpTestTimeout,
		ElectionTimeout:    tcpTestTimeout,
		LeaderLeaseTimeout: tcpTestTimeout / 2,
		CommitTimeout:      5 * time.Millisecond,
		ApplyTimeout:       2 * time.Second,
	})
	if err != nil {
		c.t.Fatalf("NewNode(%s): %v", id, err)
	}
	c.nodes[id] = n
	return n
}

func (c *tcpCluster) stop(id string) {
	c.t.Helper()
	if err := c.nodes[id].Close(); err != nil {
		c.t.Fatalf("Close(%s): %v", id, err)
	}
	c.nodes[id] = nil
}

func (c *tcpCluster) live(exclude ...string) []*Node {
	skip := map[string]bool{}
	for _, id := range exclude {
		skip[id] = true
	}
	var out []*Node
	for _, id := range c.ids {
		if n := c.nodes[id]; n != nil && !skip[id] {
			out = append(out, n)
		}
	}
	return out
}

func (c *tcpCluster) waitForLeader(exclude ...string) *Node {
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

// tcpTestTimeout is the heartbeat and election timeout of tcpCluster nodes.
const tcpTestTimeout = 400 * time.Millisecond

// applyOnLeader runs apply on the current leader of c until its replication
// outcome is known and returns the leader that settled it together with the
// state machine error (nil on success).
//
// Retry policy, following the documented Apply contract:
//   - ErrNotLeader: the command was rejected before it entered the log, so
//     it is retried unchanged on the next leader.
//   - ErrOutcomeUnknown: the command may or may not commit. The helper waits
//     for a leader, runs a Barrier there so every committed entry is applied
//     locally, and asks applied whether the effect is present. Only if it is
//     not, the command is sent again. A nil applied makes the unknown outcome
//     fatal. Callers must give a predicate that tells "already applied" from
//     "not applied", so a non-idempotent command is never applied twice.
//
// Any other error is a state machine result and is returned unchanged.
func (c *tcpCluster) applyOnLeader(what string, apply func(*Node) error, applied func(*Node) bool) (*Node, error) {
	c.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: no settled outcome after %d attempts within %s", what, attempt-1, waitTimeout)
		}
		leader := c.waitForLeader()
		err := apply(leader)
		switch {
		case err == nil:
			return leader, nil
		case errors.Is(err, ErrNotLeader):
			c.t.Logf("%s on %s: %v; retrying on the next leader", what, leader.id, err)
			continue
		case errors.Is(err, ErrOutcomeUnknown):
			if applied == nil {
				c.t.Fatalf("%s on %s: %v (no reconciliation predicate)", what, leader.id, err)
			}
			c.t.Logf("%s on %s: %v; reconciling", what, leader.id, err)
			l := c.barrierLeader()
			if applied(l) {
				return l, nil
			}
			continue
		default:
			return leader, err
		}
	}
}

// resendRejected is the reconciliation predicate for a command the journal
// is expected to reject (ErrNotOwner): such a command changes no state
// whether or not it committed, so an unknown outcome is settled by sending it
// again until the state machine's answer is observed.
func resendRejected(*Node) bool { return false }

// barrierLeader waits for a leader and runs a Barrier on it. Barrier only
// waits for already committed entries, so it is retried on ErrNotLeader.
func (c *tcpCluster) barrierLeader() *Node {
	c.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		leader := c.waitForLeader()
		err := leader.Barrier(5 * time.Second)
		if err == nil {
			return leader
		}
		if !errors.Is(err, ErrNotLeader) || time.Now().After(deadline) {
			c.t.Fatalf("Barrier on %s: %v", leader.id, err)
		}
		c.t.Logf("Barrier on %s: %v; retrying on the next leader", leader.id, err)
	}
}

// mustApplyJournal applies a journal command through applyOnLeader and fails
// on a state machine error. It returns the leader that settled the command.
func (c *tcpCluster) mustApplyJournal(cmd Command) *Node {
	c.t.Helper()
	what := fmt.Sprintf("Apply(%s %s %s)", cmd.Kind, cmd.OpID, cmd.StepID)
	l, err := c.applyOnLeader(what, func(n *Node) error { return n.Apply(cmd) }, journalApplied(cmd))
	if err != nil {
		c.t.Fatalf("%s on %s: %v", what, l.id, err)
	}
	return l
}

// journalApplied reports whether the effect of cmd is visible in a node's
// journal. It covers the commands the TCP tests issue.
func journalApplied(cmd Command) func(*Node) bool {
	return func(n *Node) bool {
		op, err := n.Journal().Get(cmd.OpID)
		if err != nil {
			return false
		}
		step := func() opjournal.State {
			for _, s := range op.Steps {
				if s.ID == cmd.StepID {
					return s.State
				}
			}
			return ""
		}
		switch cmd.Kind {
		case "begin":
			return true
		case "start":
			return step() == opjournal.Running
		case "finish":
			return step() == cmd.Result
		case "adopt":
			return op.Owner >= cmd.Term
		case "abort":
			for _, u := range n.Journal().Unfinished() {
				if u.ID == cmd.OpID {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
}

func journalBytes(t *testing.T, n *Node) []byte {
	t.Helper()
	b, err := n.Journal().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// waitForJournal waits until every node's journal snapshot equals want.
func waitForJournal(t *testing.T, nodes []*Node, want []byte) {
	t.Helper()
	for _, n := range nodes {
		waitFor(t, waitTimeout, "journal on "+n.id, func() bool { return bytes.Equal(journalBytes(t, n), want) })
	}
}

func TestTCPClusterRestartsFromDisk(t *testing.T) {
	c := newTCPCluster(t, 3)
	term := c.waitForLeader().Term()

	// The operations stay owned by term even if leadership moves while they
	// are written: the journal fences by owner term, not by the current one.
	c.mustApplyJournal(Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: []string{"a", "b"}, Term: term})
	c.mustApplyJournal(Command{Kind: "start", OpID: "op1", StepID: "a", Term: term})
	c.mustApplyJournal(Command{Kind: "finish", OpID: "op1", StepID: "a", Result: opjournal.Succeeded, Term: term})
	leader := c.mustApplyJournal(Command{Kind: "begin", OpID: "op2", Steps: []string{"x"}, Term: term})
	waitForJournal(t, c.live(), journalBytes(t, leader))

	// Snapshot every node, then commit more entries so a restart restores
	// from the file snapshot and replays the log after it.
	for _, n := range c.live() {
		if err := n.raft.Snapshot().Error(); err != nil {
			t.Fatalf("snapshot %s: %v", n.id, err)
		}
	}
	c.mustApplyJournal(Command{Kind: "start", OpID: "op1", StepID: "b", Term: term})
	c.mustApplyJournal(Command{Kind: "abort", OpID: "op2", Term: term})
	leader = c.mustApplyJournal(Command{Kind: "begin", OpID: "op3", Steps: []string{"y"}, Term: term})
	want := journalBytes(t, leader)
	waitForJournal(t, c.live(), want)

	terms := map[string]uint64{}
	for _, id := range c.ids {
		terms[id] = c.nodes[id].Term()
		c.stop(id)
	}
	for _, id := range c.ids {
		for _, f := range []string{"raft.db", "snapshots"} {
			if _, err := os.Stat(filepath.Join(c.dirs[id], f)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}

	for _, id := range c.ids {
		n := c.start(id)
		// The term comes from the stable store before any new election.
		if got := n.Term(); got < terms[id] {
			t.Fatalf("%s restarted with term %d, below %d before stop", id, got, terms[id])
		}
	}
	leader = c.barrierLeader()
	if leader.Term() <= term {
		t.Fatalf("leader term %d after restart not above %d", leader.Term(), term)
	}
	if got := journalBytes(t, leader); !bytes.Equal(got, want) {
		t.Fatalf("leader journal after restart and barrier:\n got %s\nwant %s", got, want)
	}
	waitForJournal(t, c.live(), want)

	// The restored cluster keeps enforcing ownership and accepts new work.
	newTerm := leader.Term()
	cmd := Command{Kind: "start", OpID: "op3", StepID: "y", Term: newTerm}
	if _, err := c.applyOnLeader("start op3 with non-owner term", func(n *Node) error { return n.Apply(cmd) }, resendRejected); !errors.Is(err, opjournal.ErrNotOwner) {
		t.Fatalf("start with non-owner term after restart: got %v, want ErrNotOwner", err)
	}
	c.mustApplyJournal(Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool {
		return op.Owner == newTerm && op.Steps[1].State == opjournal.Unknown
	})
}

func TestTCPLeaderRestartConverges(t *testing.T) {
	c := newTCPCluster(t, 3)
	old := c.waitForLeader()
	oldTerm := old.Term()
	old = c.mustApplyJournal(Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: oldTerm})
	waitForJournal(t, c.live(), journalBytes(t, old))

	// Stop whichever node leads now; its last term is the one to fence.
	oldID, stoppedTerm := old.id, old.Term()
	c.stop(oldID)
	leader := c.waitForLeader(oldID)
	if leader.Term() <= stoppedTerm {
		t.Fatalf("new term %d not greater than old term %d", leader.Term(), stoppedTerm)
	}
	leader = c.barrierLeader()
	newTerm := leader.Term()
	c.mustApplyJournal(Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	c.mustApplyJournal(Command{Kind: "start", OpID: "op1", StepID: "a", Term: newTerm})
	leader = c.mustApplyJournal(Command{Kind: "begin", OpID: "op2", Steps: []string{"x"}, Term: newTerm})
	want := journalBytes(t, leader)
	waitForJournal(t, c.live(), want)

	restarted := c.start(oldID)
	waitForJournal(t, []*Node{restarted}, want)
	waitFor(t, waitTimeout, "restarted node to catch up on the term", func() bool { return restarted.Term() >= newTerm })
	if restarted.IsLeader() && restarted.Term() == stoppedTerm {
		t.Fatal("restarted node leads in its old term")
	}
	// The old term is fenced for the adopted operation.
	cmd := Command{Kind: "finish", OpID: "op1", StepID: "a", Result: opjournal.Succeeded, Term: oldTerm}
	if _, err := c.applyOnLeader("finish op1 with old term", func(n *Node) error { return n.Apply(cmd) }, resendRejected); !errors.Is(err, opjournal.ErrNotOwner) {
		t.Fatalf("finish with old term: got %v, want ErrNotOwner", err)
	}
}

func TestBarrierMakesNewLeaderJournalComplete(t *testing.T) {
	c := newTestCluster(t, 3)
	old := c.waitForLeader()
	term := old.Term()
	const ops = 25
	for i := 0; i < ops; i++ {
		mustApply(t, old, Command{Kind: "begin", OpID: fmt.Sprintf("op%02d", i), Steps: []string{"a"}, Term: term})
	}
	want := journalBytes(t, old)

	c.stop(old.id)
	// Poll without sleeping so the check runs as close as possible to the
	// moment of election, before the new leader's first entry commits.
	var leader *Node
	deadline := time.Now().Add(waitTimeout)
	for leader == nil {
		if time.Now().After(deadline) {
			t.Fatal("no new leader")
		}
		for _, n := range c.live() {
			if n.IsLeader() {
				leader = n
			}
		}
		runtime.Gosched()
	}
	// Without this barrier the comparison below failed in 19 of 20 runs
	// (measured while writing the test): the new leader had not yet applied
	// the entries it holds.
	if err := leader.Barrier(5 * time.Second); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	// No waiting: after the barrier every committed entry is applied locally.
	if got := journalBytes(t, leader); !bytes.Equal(got, want) {
		t.Fatalf("journal after barrier:\n got %s\nwant %s", got, want)
	}
	if n := len(leader.Journal().Unfinished()); n != ops {
		t.Fatalf("unfinished operations after barrier: %d, want %d", n, ops)
	}
}

func TestBarrierOnFollowerReturnsErrNotLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	if err := c.follower(leader).Barrier(time.Second); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Barrier on follower: got %v, want ErrNotLeader", err)
	}
}

func TestNewNodeValidatesTransportConfig(t *testing.T) {
	if _, err := NewNode(Config{ID: "n"}); err == nil {
		t.Fatal("no transport and no bind address: got nil error")
	}
	tr := &partTransport{net: &partitionNet{}}
	if _, err := NewNode(Config{ID: "n", Transport: tr, BindAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("transport and bind address: got nil error")
	}
	if _, err := NewNode(Config{ID: "n", BindAddr: "0.0.0.0:0"}); err == nil {
		t.Fatal("unadvertisable bind address without AdvertiseAddr: got nil error")
	}
}

func TestDiskNodeReleasesDataDirOnClose(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ID: "solo", BindAddr: freeLoopbackAddr(t), DataDir: dir, Bootstrap: true,
		HeartbeatTimeout: 50 * time.Millisecond, ElectionTimeout: 50 * time.Millisecond,
		LeaderLeaseTimeout: 50 * time.Millisecond}
	cfg.Peers = []Peer{{ID: "solo", Address: cfg.BindAddr}}
	for i := 0; i < 2; i++ {
		n, err := NewNode(cfg)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		waitFor(t, waitTimeout, "single node leader", n.IsLeader)
		if i == 0 {
			mustApply(t, n, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: n.Term()})
		} else {
			if err := n.Barrier(5 * time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := n.Journal().Get("op1"); err != nil {
				t.Fatalf("after restart: %v", err)
			}
		}
		if err := n.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestSecondNodeOnSameDataDirFails(t *testing.T) {
	old := boltOpenTimeout
	boltOpenTimeout = 200 * time.Millisecond
	t.Cleanup(func() { boltOpenTimeout = old })

	dir := t.TempDir()
	first, err := NewNode(Config{ID: "a", BindAddr: freeLoopbackAddr(t), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	bind := freeLoopbackAddr(t)
	n, err := NewNode(Config{ID: "b", BindAddr: bind, DataDir: dir})
	if err == nil {
		_ = n.Close()
		t.Fatal("second node on the same data directory: got nil error")
	}
	if !strings.Contains(err.Error(), "open log store") || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("second node: got %v, want a bbolt lock timeout", err)
	}
	// The failed start released its TCP listener.
	l, err := net.Listen("tcp", bind)
	if err != nil {
		t.Fatalf("bind address still in use after failed start: %v", err)
	}
	_ = l.Close()
}

func TestFSMSnapshotHoldsStateNotHistory(t *testing.T) {
	f := newFSM()
	_ = applyLog(t, f, 1, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: 1})
	size := func() int {
		snap, err := f.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		sink := &bufferSink{}
		if err := snap.Persist(sink); err != nil {
			t.Fatal(err)
		}
		snap.Release()
		// The persisted bytes are the opjournal snapshot format.
		if _, err := opjournal.Restore(sink.Bytes()); err != nil {
			t.Fatalf("persisted FSM snapshot is not an opjournal snapshot: %v", err)
		}
		return sink.Len()
	}
	before := size()
	for i := uint64(2); i < 300; i++ {
		// Rejected and state-preserving commands alike.
		_ = applyLog(t, f, i, Command{Kind: "begin", OpID: "op1", Steps: []string{"a"}, Term: 1})
		_ = applyLog(t, f, i, Command{Kind: "abort", OpID: "op1", Term: 1})
	}
	if after := size(); after-before > len(`,"aborted":true`) {
		t.Fatalf("FSM snapshot grew from %d to %d bytes", before, after)
	}
}
