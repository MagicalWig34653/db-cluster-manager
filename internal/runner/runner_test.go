// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

func svcArgs(id string) json.RawMessage {
	b, _ := json.Marshal(agent.ServiceArgs{ServiceID: id})
	return b
}

func inspectArgs(id string) json.RawMessage {
	b, _ := json.Marshal(agent.InspectArgs{ObjectKind: "service", ID: id})
	return b
}

// threeStepOp: stop-db on h1, remove-service on h2, stop-db on h3.
func threeStepOp(id string, locks ...string) Operation {
	if len(locks) == 0 {
		locks = []string{"cluster/c1"}
	}
	return Operation{ID: id, Kind: "test", Locks: locks, Steps: []Step{
		{ID: "s1", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("a"), MaxRetries: 2},
		{ID: "s2", Target: "h2", Command: agent.KindRemoveService, Args: svcArgs("b"), MaxRetries: 2},
		{ID: "s3", Target: "h3", Command: agent.KindStopDB, Args: svcArgs("c"), MaxRetries: 2},
	}}
}

func journalStates(t *testing.T, j Journal, opID string) map[string]opjournal.Step {
	t.Helper()
	op, err := j.Get(opID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]opjournal.Step{}
	for _, s := range op.Steps {
		m[s.ID] = s
	}
	return m
}

func eventTypes(v View) []EventType {
	var out []EventType
	for _, e := range v.Timeline {
		out = append(out, e.Type)
	}
	return out
}

func TestRunExecutesStepsInOrderWithTerm(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	r := h.runner("m1")
	v, err := r.Run(context.Background(), threeStepOp("op1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusSucceeded {
		t.Fatalf("status %s (%s)", v.Status, v.Reason)
	}
	for _, host := range []string{"h1", "h2", "h3"} {
		got := h.agents[host].Received()
		if len(got) != 1 || got[0].Term != 1 || got[0].OpID != "op1" {
			t.Errorf("%s received %+v", host, got)
		}
	}
	for id, s := range journalStates(t, h.meta.node("m1"), "op1") {
		if s.State != opjournal.Succeeded || s.Attempts != 1 {
			t.Errorf("step %s: %+v", id, s)
		}
	}
	// Each start is journaled before its command; finishes follow in order.
	var order []string
	for _, w := range h.meta.writes {
		order = append(order, w.Kind+":"+w.StepID)
	}
	want := []string{"begin:", "start:s1", "finish:s1", "start:s2", "finish:s2", "start:s3", "finish:s3"}
	if !slices.Equal(order, want) {
		t.Errorf("journal writes %v, want %v", order, want)
	}
	types := eventTypes(v)
	if types[0] != EventBegin || types[len(types)-1] != EventSucceed {
		t.Errorf("timeline %v", types)
	}
	for i := 1; i < len(v.Timeline); i++ {
		if !v.Timeline[i].At.After(v.Timeline[i-1].At) {
			t.Fatalf("timeline not ordered by injected clock")
		}
	}
	if len(v.Steps) != 3 || v.Steps[1].LastResult == nil || v.Steps[1].LastResult.Status != agent.StatusSucceeded {
		t.Errorf("step views %+v", v.Steps)
	}
}

func TestRunRefusedWithoutLeadershipOrBarrier(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	if _, err := h.runner("m2").Run(context.Background(), threeStepOp("op1")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower: err %v", err)
	}
	h.meta.barrierErr["m1"] = errors.New("barrier timeout")
	if _, err := h.runner("m1").Run(context.Background(), threeStepOp("op2")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("barrier failure: err %v", err)
	}
	if n := len(h.allReceived()); n != 0 {
		t.Errorf("%d commands sent", n)
	}
	if len(h.meta.writes) != 0 {
		t.Errorf("journal writes %+v", h.meta.writes)
	}
}

func TestLostLeadershipStopsBeforeNextCommand(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.meta.afterWrite = func(w write) {
		if w.Kind == "finish" && w.StepID == "s1" {
			h.meta.elect("m2", false)
		}
	}
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if !errors.Is(err, ErrLostLeadership) || v.Status != StatusStopped {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h2"].Received()) + len(h.agents["h3"].Received()); n != 0 {
		t.Errorf("%d commands after leadership loss", n)
	}
	if len(v.Locks) == 0 {
		t.Fatal("view lost its lock keys")
	}
}

func TestJournalOutcomeUnknownStops(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.meta.failWrite = func(w write) error {
		if w.Kind == "start" && w.StepID == "s2" {
			return metastore.ErrOutcomeUnknown
		}
		return nil
	}
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if !errors.Is(err, ErrLostLeadership) || v.Status != StatusStopped {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h2"].Received()); n != 0 {
		t.Errorf("command sent although StartStep was not confirmed")
	}
}

func TestDroppedResponseAdoptsRecordedOutcome(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", dropResponse)
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if err != nil || v.Status != StatusSucceeded {
		t.Fatalf("err %v status %s (%s)", err, v.Status, v.Reason)
	}
	got := h.agents["h2"].Received()
	if len(got) != 2 || got[0].StepID != "s2" || got[1].StepID != "s2" {
		t.Fatalf("h2 received %+v; want the same step ID twice", got)
	}
	if eff := h.agents["h2"].Effects(); len(eff) != 1 {
		t.Errorf("effects %v; want exactly one", eff)
	}
	if s := journalStates(t, h.meta.node("m1"), "op1")["s2"]; s.Attempts != 2 || s.State != opjournal.Succeeded {
		t.Errorf("journal s2 %+v", s)
	}
	finishes := h.meta.writesOf("finish")
	if finishes[1].StepID != "s2" || finishes[1].Result != opjournal.Unknown {
		t.Errorf("first s2 finish %+v; want Unknown, never Failed", finishes[1])
	}
	if !slices.Contains(eventTypes(v), EventQuery) || !v.Steps[1].LastResult.Replayed {
		t.Errorf("no query/replayed result in view: %v", eventTypes(v))
	}
}

func TestInDoubtDestructiveStepPausesThenReconciles(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", crashInDoubt)
	r := h.runner("m1")
	v, err := r.Run(context.Background(), threeStepOp("op1"))
	if err != nil || v.Status != StatusPaused || v.PausedStep != "s2" {
		t.Fatalf("err %v status %s step %q", err, v.Status, v.PausedStep)
	}
	got := h.agents["h2"].Received()
	if len(got) != 2 || got[1].StepID != "s2" {
		t.Fatalf("h2 received %+v; want one query with the same step ID", got)
	}
	if len(h.agents["h3"].Received()) != 0 {
		t.Fatal("next step ran while paused")
	}
	if !strings.Contains(v.Reason, "in doubt") {
		t.Errorf("reason %q", v.Reason)
	}
	// Resume re-queries and pauses again: no blind retry.
	v, err = r.Resume(context.Background(), "op1")
	if err != nil || v.Status != StatusPaused {
		t.Fatalf("resume: err %v status %s", err, v.Status)
	}
	if eff := h.agents["h2"].Effects(); len(eff) != 1 {
		t.Fatalf("effects %v", eff)
	}
	if _, err := r.Reconcile(context.Background(), "op1", "s1", ResolveApplied); !errors.Is(err, ErrNotPaused) {
		t.Fatalf("reconcile of a non-paused step: %v", err)
	}
	v, err = r.Reconcile(context.Background(), "op1", "s2", ResolveNotApplied)
	if err != nil || v.Status != StatusSucceeded {
		t.Fatalf("reconcile: err %v status %s (%s)", err, v.Status, v.Reason)
	}
	got = h.agents["h2"].Received()
	last := got[len(got)-1]
	if last.StepID == "s2" || !strings.HasPrefix(last.StepID, "s2~r") {
		t.Errorf("reconciled retry used step ID %q; want a new one", last.StepID)
	}
}

func TestReconcileAppliedSendsNothing(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", crashInDoubt)
	r := h.runner("m1")
	if v, _ := r.Run(context.Background(), threeStepOp("op1")); v.Status != StatusPaused {
		t.Fatalf("status %s", v.Status)
	}
	before := len(h.agents["h2"].Received())
	v, err := r.Reconcile(context.Background(), "op1", "s2", ResolveApplied)
	if err != nil || v.Status != StatusSucceeded {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if len(h.agents["h2"].Received()) != before {
		t.Error("ResolveApplied sent a command")
	}
}

func TestUnreachableAgentBoundedQueriesThenPause(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].setUnreachable(true)
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if err != nil || v.Status != StatusPaused || v.PausedStep != "s2" {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	got := h.agents["h2"].Received()
	if len(got) != 3 {
		t.Fatalf("h2 received %d commands; want 1 + MaxRetries(2)", len(got))
	}
	for _, c := range got {
		if c.StepID != "s2" {
			t.Errorf("query used step ID %q", c.StepID)
		}
	}
	if s := h.sleeper.Slept(); !slices.Equal(s, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("backoff %v", s)
	}
	if s := journalStates(t, h.meta.node("m1"), "op1")["s2"]; s.State != opjournal.Unknown {
		t.Errorf("journal s2 %s; want unknown", s.State)
	}
}

func TestIdempotentKindRetriesWithNewStepIDs(t *testing.T) {
	h := newHarness(t, "h1")
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"cluster/c1"}, Steps: []Step{
		{ID: "look", Target: "h1", Command: agent.KindInspect, Args: inspectArgs("a"), MaxRetries: 3},
	}}
	h.agents["h1"].addFault("look", failResult)
	h.agents["h1"].addFault("look~a2", crashInDoubt)
	v, err := h.runner("m1").Run(context.Background(), op)
	if err != nil || v.Status != StatusSucceeded {
		t.Fatalf("err %v status %s (%s)", err, v.Status, v.Reason)
	}
	var ids []string
	for _, c := range h.agents["h1"].Received() {
		ids = append(ids, c.StepID)
	}
	// failed -> new ID; no answer -> query the same ID (in doubt) -> new ID.
	// Suffixes follow the journal attempt counter, so they never repeat.
	if !slices.Equal(ids, []string{"look", "look~a2", "look~a2", "look~a4"}) {
		t.Errorf("step IDs %v", ids)
	}
	if s := h.sleeper.Slept(); !slices.Equal(s, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Errorf("backoff %v", s)
	}
}

func TestIdempotentRetriesAreBounded(t *testing.T) {
	h := newHarness(t, "h1")
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"x"}, Steps: []Step{
		{ID: "look", Target: "h1", Command: agent.KindInspect, Args: inspectArgs("a"), MaxRetries: 1},
	}}
	h.agents["h1"].addFault("look", failResult)
	h.agents["h1"].addFault("look~a2", failResult)
	v, err := h.runner("m1").Run(context.Background(), op)
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h1"].Received()); n != 2 {
		t.Errorf("%d attempts", n)
	}
}

func TestDestructiveFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", failResult)
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h2"].Received()); n != 1 {
		t.Errorf("destructive step sent %d times", n)
	}
	if len(h.agents["h3"].Received()) != 0 {
		t.Error("ran past a failed step")
	}
}

// TestLeaderChangeAdoptsAndFencesOldTerm: m1 loses the answer of s2, then m2
// is elected. m1 keeps believing it leads (partition) and tries to continue;
// its journal writes and its commands are rejected. m2 adopts, queries s2,
// adopts the recorded result and finishes with its own term.
func TestLeaderChangeAdoptsAndFencesOldTerm(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", dropResponse)
	op := threeStepOp("op1")
	r1 := h.runner("m1")
	h.agents["h2"].before = func(cmd agent.Command) {
		if cmd.Term == 1 && cmd.StepID == "s2" {
			h.agents["h2"].before = nil
			h.meta.elect("m2", true)
		}
	}
	v1, err := r1.Run(context.Background(), op)
	if !errors.Is(err, ErrLostLeadership) || v1.Status != StatusStopped {
		t.Fatalf("old leader: err %v status %s", err, v1.Status)
	}
	if s := journalStates(t, h.meta.node("m2"), "op1")["s2"]; s.State != opjournal.Running {
		t.Fatalf("s2 %s; want still running before adoption", s.State)
	}

	r2 := h.runner("m2")
	ids, err := r2.TakeOver(func(id string) (Operation, bool) { return op, id == "op1" })
	if err != nil || !slices.Equal(ids, []string{"op1"}) {
		t.Fatalf("takeover %v %v", ids, err)
	}
	if s := journalStates(t, h.meta.node("m2"), "op1")["s2"]; s.State != opjournal.Unknown {
		t.Fatalf("after adopt s2 is %s; want unknown", s.State)
	}
	v2, err := r2.Resume(context.Background(), "op1")
	if err != nil || v2.Status != StatusSucceeded || v2.Term != 2 {
		t.Fatalf("new leader: err %v status %s term %d (%s)", err, v2.Status, v2.Term, v2.Reason)
	}
	if eff := h.agents["h2"].Effects(); len(eff) != 1 {
		t.Errorf("h2 effects %v; want s2 exactly once", eff)
	}
	if c := h.agents["h3"].Received(); len(c) != 1 || c[0].Term != 2 {
		t.Errorf("h3 received %+v", c)
	}

	// A delayed command of the old term is rejected by the agent fence.
	_, err = h.agents["h3"].Handle(context.Background(), agent.Command{Term: 1, OpID: "op1", StepID: "late", Kind: agent.KindStopDB, Args: svcArgs("c")})
	if !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("stale command: %v", err)
	}
	// The old runner cannot resume: Barrier fails on a non-leader.
	if _, err := r1.Resume(context.Background(), "op1"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("old runner resume: %v", err)
	}
}

// TestStaleLeaderCommandRejectedByAgent: m1 journaled StartStep at term 1,
// then m2 was elected and reached the agent first; m1's command is fenced.
func TestStaleLeaderCommandRejectedByAgent(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.meta.afterWrite = func(w write) {
		if w.Kind == "start" && w.StepID == "s2" && w.Term == 1 {
			h.meta.elect("m2", true)
			if _, err := h.agents["h2"].Handle(context.Background(), agent.Command{Term: 2, OpID: "probe", StepID: "p", Kind: agent.KindInspect, Args: inspectArgs("x")}); err != nil {
				t.Errorf("probe: %v", err)
			}
		}
	}
	v, err := h.runner("m1").Run(context.Background(), threeStepOp("op1"))
	if !errors.Is(err, ErrLostLeadership) || v.Status != StatusStopped {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	for _, e := range h.agents["h2"].Effects() {
		if strings.HasPrefix(e, "op1/") {
			t.Errorf("stale effect %s", e)
		}
	}
}

func TestTakeOverWithoutDefinitionOnlyAborts(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].setUnreachable(true)
	if v, _ := h.runner("m1").Run(context.Background(), threeStepOp("op1")); v.Status != StatusPaused {
		t.Fatalf("status %s", v.Status)
	}
	h.meta.elect("m2", false)
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Resume(context.Background(), "op1"); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("resume without definition: %v", err)
	}
	if err := r2.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	if u, _ := h.meta.node("m2").Unfinished(); len(u) != 0 {
		t.Errorf("unfinished after abort: %v", u)
	}
}

func TestAbortRequestedDuringStepTakesEffectAtBoundary(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	entered, release := h.agents["h1"].block("s1")
	r := h.runner("m1")
	done := make(chan View)
	go func() {
		v, _ := r.Run(context.Background(), threeStepOp("op1"))
		done <- v
	}()
	<-entered
	if err := r.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Snapshot("op1"); !v.AbortRequested || v.Status != StatusRunning {
		t.Errorf("during step: %+v", v.Status)
	}
	release()
	v := <-done
	if v.Status != StatusAborted {
		t.Fatalf("status %s", v.Status)
	}
	if s := journalStates(t, h.meta.node("m1"), "op1")["s1"]; s.State != opjournal.Succeeded {
		t.Errorf("s1 %s; the running step must complete", s.State)
	}
	if len(h.agents["h2"].Received()) != 0 {
		t.Error("step after abort was sent")
	}
	if _, err := r.Resume(context.Background(), "op1"); !errors.Is(err, ErrFinished) {
		t.Errorf("resume aborted: %v", err)
	}
}

// TestAbortDuringPreconditionWait: an abort requested while a step waits
// for its preconditions takes effect at the next poll, not after all
// polls (lab E4: a backup gate kept the operation for minutes).
func TestAbortDuringPreconditionWait(t *testing.T) {
	h := newHarness(t, "h1")
	h.checker = checkerFunc(func(context.Context, string, Step, Condition) (bool, string, error) { return false, "not yet", nil })
	r := h.runner("m1")
	h.sleeper.hook = func(n int) {
		if n == 2 {
			if err := r.Abort("op1"); err != nil {
				t.Error(err)
			}
		}
	}
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"x"}, Steps: []Step{
		{ID: "gate", Preconditions: []Condition{{Kind: "quorum", ClusterSize: 2}}, MaxRetries: 30},
		{ID: "act", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("a")},
	}}
	v, err := r.Run(context.Background(), op)
	if err != nil || v.Status != StatusAborted {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.sleeper.Slept()); n > 3 {
		t.Errorf("%d polls after the abort request", n)
	}
	if len(h.agents["h1"].Received()) != 0 {
		t.Error("step after the gate was sent")
	}
}

func TestAbortPausedOperationReleasesLocks(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", crashInDoubt)
	r := h.runner("m1")
	if v, _ := r.Run(context.Background(), threeStepOp("op1")); v.Status != StatusPaused {
		t.Fatalf("status %s", v.Status)
	}
	// A paused operation keeps its lock.
	if _, err := r.Run(context.Background(), threeStepOp("op2")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second op while paused: %v", err)
	}
	if err := r.Abort("op1"); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Run(context.Background(), threeStepOp("op2")); err != nil || v.Status != StatusSucceeded {
		t.Fatalf("after abort: %v %s", err, v.Status)
	}
}

func TestLocksSerializeOperationsOnSameResource(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3", "h4")
	entered, release := h.agents["h1"].block("s1")
	r := h.runner("m1")
	var wg sync.WaitGroup
	results := make(chan View, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, _ := r.Run(context.Background(), threeStepOp("op1", "cluster/c1", "node/c1/n1"))
		results <- v
	}()
	<-entered

	// Many concurrent attempts on the same node: all refused.
	var busy sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		busy.Add(1)
		go func(i int) {
			defer busy.Done()
			op := threeStepOp("other"+string(rune('a'+i)), "node/c1/n1")
			_, err := r.Run(context.Background(), op)
			errs <- err
		}(i)
	}
	busy.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrBusy) {
			t.Errorf("concurrent op on locked node: %v", err)
		}
	}
	// A disjoint operation runs meanwhile.
	disjoint := Operation{ID: "op3", Kind: "test", Locks: []string{"cluster/c2"}, Steps: []Step{
		{ID: "x", Target: "h4", Command: agent.KindStopDB, Args: svcArgs("z")},
	}}
	if v, err := r.Run(context.Background(), disjoint); err != nil || v.Status != StatusSucceeded {
		t.Fatalf("disjoint: %v %s", err, v.Status)
	}
	release()
	wg.Wait()
	if v := <-results; v.Status != StatusSucceeded {
		t.Fatalf("op1 %s", v.Status)
	}
	if v, err := r.Run(context.Background(), threeStepOp("op2", "node/c1/n1")); err != nil || v.Status != StatusSucceeded {
		t.Fatalf("after release: %v", err)
	}
}

func TestPreconditionPollsThenPausesAndResumes(t *testing.T) {
	h := newHarness(t, "h1")
	var mu sync.Mutex
	synced := false
	h.checker = checkerFunc(func(_ context.Context, _ string, _ Step, c Condition) (bool, string, error) {
		mu.Lock()
		defer mu.Unlock()
		return synced, "node not synced", nil
	})
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"x"}, Steps: []Step{
		{ID: "wait", MaxRetries: 2, Preconditions: []Condition{{Kind: "node-synced", Target: "n1", ClusterSize: 1}}},
		{ID: "act", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("a"), Preconditions: []Condition{{Kind: CondStepSucceeded, StepID: "wait"}}},
	}}
	r := h.runner("m1")
	v, err := r.Run(context.Background(), op)
	if err != nil || v.Status != StatusPaused || v.PausedStep != "wait" || !strings.Contains(v.Reason, "precondition") {
		t.Fatalf("err %v %s %q", err, v.Status, v.Reason)
	}
	if n := len(h.sleeper.Slept()); n != 2 {
		t.Errorf("polls %d", n)
	}
	if len(h.agents["h1"].Received()) != 0 {
		t.Fatal("command sent before precondition")
	}
	mu.Lock()
	synced = true
	mu.Unlock()
	if v, err := r.Resume(context.Background(), "op1"); err != nil || v.Status != StatusSucceeded {
		t.Fatalf("resume: %v %s", err, v.Status)
	}
}

func TestPreconditionWithoutCheckerFailsClosed(t *testing.T) {
	h := newHarness(t, "h1")
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"x"}, Steps: []Step{
		{ID: "act", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("a"), Preconditions: []Condition{{Kind: "quorum", ClusterSize: 2}}},
	}}
	v, _ := h.runner("m1").Run(context.Background(), op)
	if v.Status != StatusPaused || len(h.agents["h1"].Received()) != 0 {
		t.Fatalf("status %s", v.Status)
	}
}

func TestValidate(t *testing.T) {
	good := threeStepOp("op")
	cases := map[string]func(*Operation){
		"no id":          func(o *Operation) { o.ID = "" },
		"no locks":       func(o *Operation) { o.Locks = nil },
		"dup step":       func(o *Operation) { o.Steps[1].ID = "s1" },
		"reserved sep":   func(o *Operation) { o.Steps[0].ID = "s~1" },
		"no target":      func(o *Operation) { o.Steps[0].Target = "" },
		"forward cond":   func(o *Operation) { o.Steps[0].Preconditions = []Condition{{Kind: CondStepSucceeded, StepID: "s2"}} },
		"negative retry": func(o *Operation) { o.Steps[0].MaxRetries = -1 },
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range cases {
		op := cloneOp(good)
		mut(&op)
		if err := op.Validate(); !errors.Is(err, ErrInvalidOperation) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStepConflictIsPermanentFailure(t *testing.T) {
	h := newHarness(t, "h1")
	// Pre-record a different command under the same IDs.
	if _, err := h.agents["h1"].Handle(context.Background(), agent.Command{Term: 1, OpID: "op1", StepID: "s1", Kind: agent.KindStopDB, Args: svcArgs("other")}); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "op1", Kind: "test", Locks: []string{"x"}, Steps: []Step{{ID: "s1", Target: "h1", Command: agent.KindStopDB, Args: svcArgs("a"), MaxRetries: 3}}}
	v, err := h.runner("m1").Run(context.Background(), op)
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h1"].Received()); n != 2 {
		t.Errorf("received %d", n)
	}
}
