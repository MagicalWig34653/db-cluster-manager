// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// fakeMeta simulates a replicated journal with one leader. Only the leader
// can write (like metastore.Node.Apply); a "believer" node may still think it
// is leader at an old term (a partitioned former leader) but cannot commit.
type fakeMeta struct {
	mu         sync.Mutex
	j          *opjournal.Journal
	leader     string
	term       uint64
	believers  map[string]uint64
	barrierErr map[string]error
	// failWrite, if set, may veto a write before it is applied.
	failWrite func(w write) error
	// afterWrite runs after a successful write, without locks held.
	afterWrite func(w write)
	writes     []write
}

type write struct {
	Node, Kind, OpID, StepID string
	Result                   opjournal.State
	Term                     uint64
}

func newFakeMeta(leader string) *fakeMeta {
	return &fakeMeta{j: opjournal.New(), leader: leader, term: 1, believers: map[string]uint64{}, barrierErr: map[string]error{}}
}

// elect makes id leader at the next term. With stale, the old leader keeps
// believing it leads at its old term.
func (m *fakeMeta) elect(id string, stale bool) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stale && m.leader != "" {
		m.believers[m.leader] = m.term
	}
	delete(m.believers, id)
	m.term++
	m.leader = id
	return m.term
}

func (m *fakeMeta) node(id string) *fakeNode { return &fakeNode{m: m, id: id} }

func (m *fakeMeta) writesOf(kind string) []write {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []write
	for _, w := range m.writes {
		if w.Kind == kind {
			out = append(out, w)
		}
	}
	return out
}

type fakeNode struct {
	m  *fakeMeta
	id string
}

func (n *fakeNode) IsLeader() bool {
	n.m.mu.Lock()
	defer n.m.mu.Unlock()
	return n.m.leader == n.id || n.m.believers[n.id] != 0
}

func (n *fakeNode) Term() uint64 {
	n.m.mu.Lock()
	defer n.m.mu.Unlock()
	if t := n.m.believers[n.id]; t != 0 {
		return t
	}
	return n.m.term
}

func (n *fakeNode) Barrier(time.Duration) error {
	n.m.mu.Lock()
	defer n.m.mu.Unlock()
	if err := n.m.barrierErr[n.id]; err != nil {
		return err
	}
	if n.m.leader != n.id {
		return metastore.ErrNotLeader
	}
	return nil
}

func (n *fakeNode) do(w write, apply func(j *opjournal.Journal) error) error {
	w.Node = n.id
	n.m.mu.Lock()
	if n.m.leader != n.id {
		n.m.mu.Unlock()
		return metastore.ErrNotLeader
	}
	if f := n.m.failWrite; f != nil {
		if err := f(w); err != nil {
			n.m.mu.Unlock()
			return err
		}
	}
	err := apply(n.m.j)
	if err == nil {
		n.m.writes = append(n.m.writes, w)
	}
	hook := n.m.afterWrite
	n.m.mu.Unlock()
	if err == nil && hook != nil {
		hook(w)
	}
	return err
}

func (n *fakeNode) Begin(opID, kind string, steps []string, term uint64) error {
	return n.do(write{Kind: "begin", OpID: opID, Term: term}, func(j *opjournal.Journal) error { return j.Begin(opID, kind, steps, term) })
}

func (n *fakeNode) StartStep(opID, stepID string, term uint64) error {
	return n.do(write{Kind: "start", OpID: opID, StepID: stepID, Term: term}, func(j *opjournal.Journal) error { return j.StartStep(opID, stepID, term) })
}

func (n *fakeNode) FinishStep(opID, stepID string, s opjournal.State, term uint64) error {
	return n.do(write{Kind: "finish", OpID: opID, StepID: stepID, Result: s, Term: term}, func(j *opjournal.Journal) error { return j.FinishStep(opID, stepID, s, term) })
}

func (n *fakeNode) Adopt(opID string, term uint64) error {
	return n.do(write{Kind: "adopt", OpID: opID, Term: term}, func(j *opjournal.Journal) error { return j.Adopt(opID, term) })
}

func (n *fakeNode) Abort(opID string, term uint64) error {
	return n.do(write{Kind: "abort", OpID: opID, Term: term}, func(j *opjournal.Journal) error { return j.Abort(opID, term) })
}

func (n *fakeNode) Get(opID string) (opjournal.Operation, error) { return n.m.j.Get(opID) }

func (n *fakeNode) Unfinished() ([]opjournal.Operation, error) { return n.m.j.Unfinished(), nil }

// memPersister keeps the fencing term in memory.
type memPersister struct {
	mu sync.Mutex
	t  uint64
}

func (p *memPersister) Load() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.t, nil
}

func (p *memPersister) Save(t uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.t = t
	return nil
}

type fault int

const (
	dropRequest  fault = iota + 1 // lost before the agent: no effect, error
	dropResponse                  // effect and record, but the answer is lost
	crashInDoubt                  // intent recorded, effect maybe, no result recorded
	failResult                    // StatusFailed recorded
	failInUse                     // StatusFailed with CodeVolumeInUse (no effect)
)

var errNet = errors.New("fake network: connection reset")

type agentRec struct {
	cmd    agent.Command
	result *agent.Result
}

// fakeAgent mirrors the agent executor contract: fencing first, results
// recorded per (OpID, StepID) and replayed, in-doubt steps never re-run.
type fakeAgent struct {
	name        string
	mu          sync.Mutex
	guard       *fencing.Guard
	recs        map[string]*agentRec
	received    []agent.Command
	effects     []string
	faults      map[string][]fault // by agent step ID
	unreachable bool
	blocks      map[string]chan struct{} // by agent step ID: wait before the effect
	entered     map[string]chan struct{} // closed when the blocked step is entered
	before      func(cmd agent.Command)
}

func newFakeAgent(t *testing.T, name string) *fakeAgent {
	g, err := fencing.NewGuard(&memPersister{})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAgent{name: name, guard: g, recs: map[string]*agentRec{}, faults: map[string][]fault{},
		blocks: map[string]chan struct{}{}, entered: map[string]chan struct{}{}}
}

func (a *fakeAgent) addFault(stepID string, f ...fault) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.faults[stepID] = append(a.faults[stepID], f...)
}

func (a *fakeAgent) block(stepID string) (entered <-chan struct{}, release func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, en := make(chan struct{}), make(chan struct{})
	a.blocks[stepID], a.entered[stepID] = ch, en
	var once sync.Once
	return en, func() { once.Do(func() { close(ch) }) }
}

func (a *fakeAgent) setUnreachable(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unreachable = v
}

func (a *fakeAgent) Effects() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.effects...)
}

func (a *fakeAgent) Received() []agent.Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.Command(nil), a.received...)
}

func (a *fakeAgent) Handle(ctx context.Context, cmd agent.Command) (agent.Result, error) {
	a.mu.Lock()
	a.received = append(a.received, cmd)
	hook := a.before
	a.mu.Unlock()
	if hook != nil {
		hook(cmd)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unreachable {
		return agent.Result{}, errNet
	}
	var f fault
	if q := a.faults[cmd.StepID]; len(q) > 0 {
		f, a.faults[cmd.StepID] = q[0], q[1:]
	}
	if f == dropRequest {
		return agent.Result{}, errNet
	}
	if err := a.guard.Admit(cmd.Term); err != nil {
		return agent.Result{}, err
	}
	key := cmd.OpID + "/" + cmd.StepID
	if rec := a.recs[key]; rec != nil {
		if rec.cmd.Kind != cmd.Kind || string(rec.cmd.Args) != string(cmd.Args) {
			return agent.Result{}, agent.ErrStepConflict
		}
		if rec.result == nil {
			return agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusUnknown, Code: agent.CodeInDoubt, Replayed: true}, nil
		}
		r := *rec.result
		r.Replayed = true
		return r, nil
	}
	rec := &agentRec{cmd: cmd}
	a.recs[key] = rec
	if ch := a.blocks[cmd.StepID]; ch != nil {
		close(a.entered[cmd.StepID])
		delete(a.blocks, cmd.StepID)
		a.mu.Unlock()
		<-ch
		a.mu.Lock()
	}
	a.effects = append(a.effects, key)
	res := agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}
	switch f {
	case crashInDoubt:
		return agent.Result{}, errNet
	case failResult:
		res.Status, res.Code, res.Message = agent.StatusFailed, agent.CodeDocker, "simulated failure"
	case failInUse:
		res.Status, res.Code, res.Message = agent.StatusFailed, agent.CodeVolumeInUse, "simulated volume in use"
	}
	rec.result = &res
	if f == dropResponse {
		return agent.Result{}, errNet
	}
	return res, nil
}

// fakeAgents implements AgentClient.
type fakeAgents map[string]*fakeAgent

func (f fakeAgents) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	a := f[target]
	if a == nil {
		return agent.Result{}, fmt.Errorf("no route to %s", target)
	}
	return a.Handle(ctx, cmd)
}

// fakeClock advances one second per reading.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// fakeSleeper records requested sleeps and returns at once.
type fakeSleeper struct {
	mu    sync.Mutex
	slept []time.Duration
	hook  func(n int)
}

func (s *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.slept = append(s.slept, d)
	n, hook := len(s.slept), s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return ctx.Err()
}

func (s *fakeSleeper) Slept() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.slept...)
}

type checkerFunc func(ctx context.Context, opID string, step Step, c Condition) (bool, string, error)

func (f checkerFunc) Check(ctx context.Context, opID string, step Step, c Condition) (bool, string, error) {
	return f(ctx, opID, step, c)
}

// harness wires managers m1, m2 and agents h1..hN.
type harness struct {
	t       *testing.T
	meta    *fakeMeta
	agents  fakeAgents
	sleeper *fakeSleeper
	clock   *fakeClock
	checker Checker
}

func newHarness(t *testing.T, hosts ...string) *harness {
	h := &harness{t: t, meta: newFakeMeta("m1"), agents: fakeAgents{}, sleeper: &fakeSleeper{},
		clock: &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}}
	for _, n := range hosts {
		h.agents[n] = newFakeAgent(t, n)
	}
	return h
}

func (h *harness) runner(manager string) *Runner {
	n := h.meta.node(manager)
	r, err := New(Config{Leader: n, Journal: n, Agents: h.agents, Checker: h.checker,
		Now: h.clock.Now, Sleep: h.sleeper.Sleep, StepTimeout: 10 * time.Second,
		BackoffBase: time.Second, BackoffMax: 8 * time.Second})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) allReceived() []agent.Command {
	var out []agent.Command
	for _, a := range h.agents {
		out = append(out, a.Received()...)
	}
	return out
}
