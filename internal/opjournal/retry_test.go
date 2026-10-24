// SPDX-License-Identifier: AGPL-3.0-only

package opjournal

import (
	"bytes"
	"reflect"
	"testing"
)

// TestFailForRetryAndAgentStep: a due retry and the agent step ID of the
// latest attempt are journaled, survive adoption and a snapshot, and the
// next start clears the retry mark.
func TestFailForRetryAndAgentStep(t *testing.T) {
	j := newOp(t, "op1", []string{"rm"}, 1)
	if err := j.FailForRetry("op1", "rm", 1); err == nil {
		t.Fatal("FailForRetry on a pending step: got nil error")
	}
	if err := j.StartStepAs("op1", "rm", "rm", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.FailForRetry("op1", "rm", 2); err == nil {
		t.Fatal("FailForRetry with a foreign term: got nil error")
	}
	if err := j.FailForRetry("op1", "rm", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.Adopt("op1", 2); err != nil {
		t.Fatal(err)
	}
	data, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Restore(data)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := r.Get("op1")
	want := Step{ID: "rm", State: Failed, Attempts: 1, AgentStep: "rm", Retry: true}
	if !reflect.DeepEqual(op.Steps[0], want) {
		t.Fatalf("restored %+v, want %+v", op.Steps[0], want)
	}
	if err := r.StartStepAs("op1", "rm", "rm~a2", 2); err != nil {
		t.Fatal(err)
	}
	op, _ = r.Get("op1")
	if s := op.Steps[0]; s.Retry || s.AgentStep != "rm~a2" || s.Attempts != 2 {
		t.Fatalf("after the retry started: %+v", s)
	}
	// StartStep keeps the recorded agent step ID.
	if err := r.FinishStep("op1", "rm", Unknown, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.StartStep("op1", "rm", 2); err != nil {
		t.Fatal(err)
	}
	if op, _ = r.Get("op1"); op.Steps[0].AgentStep != "rm~a2" {
		t.Fatalf("agent step %q", op.Steps[0].AgentStep)
	}
}

// TestSnapshotEncodingWithoutRetryUnchanged: steps without the new fields
// encode as before (no "AgentStep"/"Retry" keys).
func TestSnapshotEncodingWithoutRetryUnchanged(t *testing.T) {
	j := newOp(t, "op1", []string{"a"}, 1)
	data, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"AgentStep", "Retry"} {
		if bytes.Contains(data, []byte(`"`+k+`"`)) {
			t.Fatalf("snapshot %s has %q", data, k)
		}
	}
}
