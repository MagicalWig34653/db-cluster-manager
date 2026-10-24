// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// Lab E4 cleanuprestart (re-run A3): a cluster deletion removed a node's
// service, then its volume; Docker refused the volume ("volume in use")
// while Swarm removed the task container, and the runner waited to retry.
// The manager was SIGKILLed during that backoff. The new leader found the
// step journaled Failed and failed the deletion. A failure whose retry is
// due is now journaled as such and retried after the take-over.

func removeVolumeOp(id string) Operation {
	return Operation{ID: id, Kind: "test", Locks: []string{"cluster/c1"}, Steps: []Step{
		{ID: "rm", Target: "h1", Command: agent.KindRemoveVolume, Args: svcArgs("v"), MaxRetries: 5,
			RetryCodes: []agent.Code{agent.CodeVolumeInUse}},
	}}
}

// TestRetryDueSurvivesLeaderLossDuringBackoff: the leader is lost while it
// waits to retry; the new leader retries under the next agent step ID
// (never re-sending an ID), the volume is busy for two more attempts and
// then free, and the operation succeeds with the effect applied once.
func TestRetryDueSurvivesLeaderLossDuringBackoff(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].addFault("rm", failInUse)
	h.agents["h1"].addFault("rm~a2", failInUse)
	h.agents["h1"].addFault("rm~a3", failInUse)
	h.sleeper.hook = func(n int) {
		if n == 1 {
			h.meta.elect("m2", false) // the old manager is gone mid-backoff
		}
	}
	op := removeVolumeOp("op1")
	v1, err := h.runner("m1").Run(context.Background(), op)
	if !errors.Is(err, ErrLostLeadership) || v1.Status != StatusStopped {
		t.Fatalf("old leader: err %v status %s", err, v1.Status)
	}
	s := journalStates(t, h.meta.node("m2"), "op1")["rm"]
	if s.State != opjournal.Failed || !s.Retry || s.Attempts != 1 {
		t.Fatalf("journal after the loss: %+v; want failed with a retry due", s)
	}
	h.sleeper.hook = nil
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(func(id string) (Operation, bool) { return removeVolumeOp(id), id == "op1" }); err != nil {
		t.Fatal(err)
	}
	v2, err := r2.Resume(context.Background(), "op1")
	if err != nil || v2.Status != StatusSucceeded {
		t.Fatalf("new leader: err %v status %s (%s)", err, v2.Status, v2.Reason)
	}
	var ids []string
	for _, c := range h.agents["h1"].Received() {
		ids = append(ids, c.StepID)
	}
	want := []string{"rm", "rm~a2", "rm~a3", "rm~a4"}
	if len(ids) != len(want) {
		t.Fatalf("agent step IDs %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("agent step IDs %v, want %v", ids, want)
		}
	}
	// The fake counts every executed command; each agent step ID ran once.
	if eff := h.agents["h1"].Effects(); len(eff) != len(want) {
		t.Errorf("effects %v; want each attempt executed once", eff)
	}
}

// TestRetryDueBudgetCountsEarlierAttempts: the new leader does not restart
// the retry budget; attempts made by the previous owner count.
func TestRetryDueBudgetCountsEarlierAttempts(t *testing.T) {
	h := newHarness(t, "h1")
	for _, id := range []string{"rm", "rm~a2", "rm~a3", "rm~a4", "rm~a5", "rm~a6", "rm~a7"} {
		h.agents["h1"].addFault(id, failInUse)
	}
	h.sleeper.hook = func(n int) {
		if n == 3 {
			h.meta.elect("m2", false)
		}
	}
	op := removeVolumeOp("op1")
	if _, err := h.runner("m1").Run(context.Background(), op); !errors.Is(err, ErrLostLeadership) {
		t.Fatalf("old leader: %v", err)
	}
	h.sleeper.hook = nil
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(func(id string) (Operation, bool) { return removeVolumeOp(id), true }); err != nil {
		t.Fatal(err)
	}
	v, err := r2.Resume(context.Background(), "op1")
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	// MaxRetries 5: six attempts in all, three of them by the old leader.
	if n := len(h.agents["h1"].Received()); n != 6 {
		t.Errorf("%d attempts, want 6", n)
	}
}

// TestFailedStepWithoutRetryDueStaysFailedAfterTakeOver: a failure that was
// final (not retryable) is not retried by a new leader.
func TestFailedStepWithoutRetryDueStaysFailedAfterTakeOver(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].addFault("rm", failResult)
	op := removeVolumeOp("op1")
	if v, _ := h.runner("m1").Run(context.Background(), op); v.Status != StatusFailed {
		t.Fatalf("status %s", v.Status)
	}
	h.meta.elect("m2", false)
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(func(id string) (Operation, bool) { return removeVolumeOp(id), true }); err != nil {
		t.Fatal(err)
	}
	v, err := r2.Resume(context.Background(), "op1")
	if err != nil || v.Status != StatusFailed {
		t.Fatalf("err %v status %s", err, v.Status)
	}
	if n := len(h.agents["h1"].Received()); n != 1 {
		t.Errorf("%d attempts, want 1", n)
	}
}

// TestUnknownStepQueriedUnderJournaledAgentStepID: a retry attempt (rm~a2)
// whose answer was lost when the leader changed is queried by the new
// leader under rm~a2, the ID that was sent, not under the step's own ID
// (whose recorded answer is the old "volume in use").
func TestUnknownStepQueriedUnderJournaledAgentStepID(t *testing.T) {
	h := newHarness(t, "h1")
	h.agents["h1"].addFault("rm", failInUse)
	h.agents["h1"].addFault("rm~a2", dropResponse)
	h.agents["h1"].before = func(cmd agent.Command) {
		if cmd.StepID == "rm~a2" && cmd.Term == 1 {
			h.agents["h1"].before = nil
			h.meta.elect("m2", false)
		}
	}
	op := removeVolumeOp("op1")
	if _, err := h.runner("m1").Run(context.Background(), op); !errors.Is(err, ErrLostLeadership) {
		t.Fatalf("old leader: %v", err)
	}
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(func(id string) (Operation, bool) { return removeVolumeOp(id), true }); err != nil {
		t.Fatal(err)
	}
	v, err := r2.Resume(context.Background(), "op1")
	if err != nil || v.Status != StatusSucceeded {
		t.Fatalf("err %v status %s (%s)", err, v.Status, v.Reason)
	}
	got := h.agents["h1"].Received()
	if last := got[len(got)-1]; last.StepID != "rm~a2" || last.Term != 2 {
		t.Errorf("new leader sent %s at term %d; want a query of rm~a2", last.StepID, last.Term)
	}
	// rm~a2 executed once: the query replayed its recorded result.
	if eff := h.agents["h1"].Effects(); len(eff) != 2 || eff[1] != "op1/rm~a2" {
		t.Errorf("effects %v", eff)
	}
}
