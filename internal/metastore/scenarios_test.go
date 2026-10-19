// SPDX-License-Identifier: AGPL-3.0-only

package metastore

// Failure scenarios of the W03 prototype: a Raft-replicated operation journal
// driving a simulated agent that fences commands by term.

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// stableWindow is the bounded wait used to assert that a journal does not
// change. It is far longer than the test cluster's commit latency.
const stableWindow = 300 * time.Millisecond

// memPersister keeps the agent's highest term in memory.
type memPersister struct {
	mu   sync.Mutex
	term uint64
}

func (p *memPersister) Load() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.term, nil
}

func (p *memPersister) Save(term uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.term = term
	return nil
}

// simAgent is a simulated node agent: it admits commands through a fencing
// guard and counts the executions it performed.
type simAgent struct {
	guard *fencing.Guard
	mu    sync.Mutex
	execs map[string]int // key opID + "/" + stepID
}

func newSimAgent(t *testing.T) *simAgent {
	t.Helper()
	g, err := fencing.NewGuard(&memPersister{})
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return &simAgent{guard: g, execs: make(map[string]int)}
}

// Execute runs step stepID of operation opID if term is admitted.
func (a *simAgent) Execute(term uint64, opID, stepID string) error {
	if err := a.guard.Admit(term); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.execs[opID+"/"+stepID]++
	return nil
}

func (a *simAgent) executions(opID, stepID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.execs[opID+"/"+stepID]
}

// runStep journals and executes one step under term: start, agent execution,
// finish as Succeeded.
func runStep(t *testing.T, n *Node, a *simAgent, term uint64, opID, stepID string) {
	t.Helper()
	mustApply(t, n, Command{Kind: "start", OpID: opID, StepID: stepID, Term: term})
	if err := a.Execute(term, opID, stepID); err != nil {
		t.Fatalf("agent Execute(%d, %s, %s): %v", term, opID, stepID, err)
	}
	mustApply(t, n, Command{Kind: "finish", OpID: opID, StepID: stepID, Result: opjournal.Succeeded, Term: term})
}

// assertUnchanged polls the journals of nodes for stableWindow and fails if
// any of them differs from its Unfinished() snapshot taken at the start, or
// if absentOp appears.
func assertUnchanged(t *testing.T, nodes []*Node, absentOp string) {
	t.Helper()
	want := make(map[string][]opjournal.Operation)
	for _, n := range nodes {
		want[n.id] = n.Journal().Unfinished()
	}
	deadline := time.Now().Add(stableWindow)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			j := n.Journal() // re-fetch: a snapshot restore replaces it
			if got := j.Unfinished(); !reflect.DeepEqual(got, want[n.id]) {
				t.Fatalf("journal on %s changed: got %+v, want %+v", n.id, got, want[n.id])
			}
			if _, err := j.Get(absentOp); !errors.Is(err, opjournal.ErrNotFound) {
				t.Fatalf("operation %s on %s: got err %v, want ErrNotFound", absentOp, n.id, err)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitConverged waits until every node's Unfinished() equals the first one's
// and returns the common value.
func waitConverged(t *testing.T, nodes []*Node) []opjournal.Operation {
	t.Helper()
	var ref []opjournal.Operation
	waitFor(t, waitTimeout, "journals to converge", func() bool {
		ref = nodes[0].Journal().Unfinished()
		for _, n := range nodes[1:] {
			if !reflect.DeepEqual(n.Journal().Unfinished(), ref) {
				return false
			}
		}
		return true
	})
	return ref
}

func TestScenarioLeaderLossMidOperation(t *testing.T) {
	c := newTestCluster(t, 3)
	agent := newSimAgent(t)
	old := c.waitForLeader()
	oldTerm := old.Term()
	steps := []string{"s1", "s2", "s3"}

	mustApply(t, old, Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: steps, Term: oldTerm})
	runStep(t, old, agent, oldTerm, "op1", "s1")
	// Step 2 is started and dispatched, but the leader dies before it
	// records the result.
	mustApply(t, old, Command{Kind: "start", OpID: "op1", StepID: "s2", Term: oldTerm})
	if err := agent.Execute(oldTerm, "op1", "s2"); err != nil {
		t.Fatalf("agent Execute s2 under old term: %v", err)
	}
	c.stop(old.id)

	leader := c.waitForLeader()
	newTerm := leader.Term()
	if newTerm <= oldTerm {
		t.Fatalf("new term %d not greater than old term %d", newTerm, oldTerm)
	}
	waitForOp(t, []*Node{leader}, "op1", func(op opjournal.Operation) bool {
		return op.Steps[1].State == opjournal.Running
	})

	mustApply(t, leader, Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	wantAdopted := opjournal.Operation{ID: "op1", Kind: "failover", Owner: newTerm, Steps: []opjournal.Step{
		{ID: "s1", State: opjournal.Succeeded, Attempts: 1},
		{ID: "s2", State: opjournal.Unknown, Attempts: 1},
		{ID: "s3", State: opjournal.Pending},
	}}
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool { return reflect.DeepEqual(op, wantAdopted) })
	op, err := leader.Journal().Get("op1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Steps[1].State == opjournal.Succeeded {
		t.Fatal("step s2 became Succeeded without an observed result")
	}

	// Resume as a controller would: skip Succeeded steps, rerun the rest.
	for i, s := range op.Steps {
		if s.State == opjournal.Succeeded {
			continue
		}
		mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: s.ID, Term: newTerm})
		if s.ID == "s2" {
			got, err := leader.Journal().Get("op1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Steps[i].State != opjournal.Running || got.Steps[i].Attempts != 2 {
				t.Fatalf("restarted s2: got %+v, want Running with Attempts 2", got.Steps[i])
			}
		}
		if err := agent.Execute(newTerm, "op1", s.ID); err != nil {
			t.Fatalf("agent Execute(%s) under new term: %v", s.ID, err)
		}
		mustApply(t, leader, Command{Kind: "finish", OpID: "op1", StepID: s.ID, Result: opjournal.Succeeded, Term: newTerm})
	}

	wantDone := opjournal.Operation{ID: "op1", Kind: "failover", Owner: newTerm, Steps: []opjournal.Step{
		{ID: "s1", State: opjournal.Succeeded, Attempts: 1},
		{ID: "s2", State: opjournal.Succeeded, Attempts: 2},
		{ID: "s3", State: opjournal.Succeeded, Attempts: 1},
	}}
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool { return reflect.DeepEqual(op, wantDone) })
	for _, n := range c.live() {
		if u := n.Journal().Unfinished(); len(u) != 0 {
			t.Fatalf("Unfinished on %s: got %+v, want none", n.id, u)
		}
	}
	for step, want := range map[string]int{"s1": 1, "s2": 2, "s3": 1} {
		if got := agent.executions("op1", step); got != want {
			t.Errorf("agent executions of %s: got %d, want %d", step, got, want)
		}
	}
}

// partitionedOldLeader is the state after the partition scenario.
type partitionedOldLeader struct {
	c       *testCluster
	agent   *simAgent
	old     *Node
	leader  *Node
	oldTerm uint64
	newTerm uint64
}

// runPartitionedOldLeader isolates the leader in the middle of an operation,
// lets the majority take it over and checks that the old leader can neither
// drive the agent nor commit to the journal.
func runPartitionedOldLeader(t *testing.T) partitionedOldLeader {
	t.Helper()
	c := newTestCluster(t, 3)
	agent := newSimAgent(t)
	old := c.waitForLeader()
	oldTerm := old.Term()

	mustApply(t, old, Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: []string{"a", "b"}, Term: oldTerm})
	runStep(t, old, agent, oldTerm, "op1", "a")
	mustApply(t, old, Command{Kind: "start", OpID: "op1", StepID: "b", Term: oldTerm})
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool {
		return op.Steps[1].State == opjournal.Running
	})

	c.Isolate(old.id)
	// Issue a journal write right away, while the old leader still holds its
	// lease and believes it leads. It must not commit.
	believedLeader := old.IsLeader()
	staleApply := make(chan error, 1)
	go func() {
		staleApply <- old.Apply(Command{Kind: "begin", OpID: "op-stale", Steps: []string{"x"}, Term: old.Term()})
	}()

	leader := c.waitForLeader(old.id)
	newTerm := leader.Term()
	if newTerm <= oldTerm {
		t.Fatalf("new term %d not greater than old term %d", newTerm, oldTerm)
	}
	waitForOp(t, []*Node{leader}, "op1", func(op opjournal.Operation) bool {
		return op.Steps[1].State == opjournal.Running
	})
	mustApply(t, leader, Command{Kind: "adopt", OpID: "op1", Term: newTerm})
	mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: "b", Term: newTerm})
	if err := agent.Execute(newTerm, "op1", "b"); err != nil {
		t.Fatalf("agent rejected new leader's command: %v", err)
	}

	// The isolated node keeps its old term; that is what the agent must fence.
	staleTerm := old.Term()
	if staleTerm >= newTerm {
		t.Fatalf("isolated node term %d not below new term %d", staleTerm, newTerm)
	}
	if err := agent.Execute(staleTerm, "op1", "b"); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("agent Execute with old term %d: got %v, want ErrStaleTerm", staleTerm, err)
	}
	if got := agent.executions("op1", "b"); got != 1 {
		t.Fatalf("agent executions of b: got %d, want 1", got)
	}
	if got := agent.guard.Highest(); got != newTerm {
		t.Fatalf("agent highest term: got %d, want %d", got, newTerm)
	}

	var applyErr error
	select {
	case applyErr = <-staleApply:
	case <-time.After(waitTimeout):
		t.Fatal("Apply on isolated old leader did not return")
	}
	t.Logf("old leader believed it led at write time: %v; Apply error: %v", believedLeader, applyErr)
	if applyErr == nil {
		t.Fatal("Apply on isolated old leader returned nil, want an error")
	}
	majority := c.live(old.id)
	waitForOp(t, majority, "op1", func(op opjournal.Operation) bool {
		return op.Owner == newTerm && op.Steps[1].State == opjournal.Running && op.Steps[1].Attempts == 2
	})
	assertUnchanged(t, append(majority, old), "op-stale")

	return partitionedOldLeader{c: c, agent: agent, old: old, leader: leader, oldTerm: oldTerm, newTerm: newTerm}
}

func TestScenarioPartitionedOldLeader(t *testing.T) {
	runPartitionedOldLeader(t)
}

func TestScenarioMajorityLoss(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	term := leader.Term()
	mustApply(t, leader, Command{Kind: "begin", OpID: "op1", OpKind: "failover", Steps: []string{"a"}, Term: term})
	mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: "a", Term: term})
	want := opjournal.Operation{ID: "op1", Kind: "failover", Owner: term, Steps: []opjournal.Step{
		{ID: "a", State: opjournal.Running, Attempts: 1},
	}}
	waitForOp(t, c.live(), "op1", func(op opjournal.Operation) bool { return reflect.DeepEqual(op, want) })

	for _, n := range c.live(leader.id) {
		c.stop(n.id)
	}
	survivor := leader
	err := survivor.Apply(Command{Kind: "begin", OpID: "op-lost", Steps: []string{"x"}, Term: survivor.Term()})
	if err == nil {
		t.Fatal("Apply without quorum returned nil, want an error")
	}
	t.Logf("Apply without quorum: %v", err)
	assertUnchanged(t, []*Node{survivor}, "op-lost")

	waitFor(t, waitTimeout, "survivor to step down", func() bool { return !survivor.IsLeader() })
	if err := survivor.Apply(Command{Kind: "begin", OpID: "op-lost", Steps: []string{"x"}, Term: survivor.Term()}); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Apply after step-down: got %v, want ErrNotLeader", err)
	}

	// Local reads of the existing journal keep working.
	got, err := survivor.Journal().Get("op1")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get(op1) on survivor: got %+v (%v), want %+v", got, err, want)
	}
	if u := survivor.Journal().Unfinished(); !reflect.DeepEqual(u, []opjournal.Operation{want}) {
		t.Fatalf("Unfinished on survivor: got %+v, want [%+v]", u, want)
	}
}

func TestScenarioHealConverges(t *testing.T) {
	s := runPartitionedOldLeader(t)
	s.c.Heal()

	waitFor(t, waitTimeout, "old leader to adopt the new term", func() bool {
		return !s.old.IsLeader() && s.old.Term() >= s.newTerm
	})
	leader := s.c.waitForLeader()
	if leader.id == s.old.id {
		t.Fatalf("old leader %s regained leadership", s.old.id)
	}
	term := leader.Term()
	if term != s.newTerm {
		// Another election happened during the heal; a controller adopts
		// before acting, so do the same.
		t.Logf("leader term changed after heal: %d -> %d", s.newTerm, term)
		mustApply(t, leader, Command{Kind: "adopt", OpID: "op1", Term: term})
		mustApply(t, leader, Command{Kind: "start", OpID: "op1", StepID: "b", Term: term})
	}
	// Record the adopted step's result through the current leader after the
	// heal; the leader's local journal has applied it when Apply returns.
	mustApply(t, leader, Command{Kind: "finish", OpID: "op1", StepID: "b", Result: opjournal.Unknown, Term: term})

	all := s.c.live()
	if len(all) != 3 {
		t.Fatalf("live nodes: got %d, want 3", len(all))
	}
	attempts := 2
	if term != s.newTerm {
		attempts = 3
	}
	want := []opjournal.Operation{{ID: "op1", Kind: "failover", Owner: term, Steps: []opjournal.Step{
		{ID: "a", State: opjournal.Succeeded, Attempts: 1},
		{ID: "b", State: opjournal.Unknown, Attempts: attempts},
	}}}
	if got := waitConverged(t, all); !reflect.DeepEqual(got, want) {
		t.Fatalf("converged Unfinished: got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(leader.Journal().Unfinished(), want) {
		t.Fatalf("leader Unfinished: got %+v, want %+v", leader.Journal().Unfinished(), want)
	}
	for _, n := range all {
		if _, err := n.Journal().Get("op-stale"); !errors.Is(err, opjournal.ErrNotFound) {
			t.Fatalf("op-stale on %s after heal: got err %v, want ErrNotFound", n.id, err)
		}
	}
}
