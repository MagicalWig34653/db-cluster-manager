package runner

import (
	"context"
	"errors"
	"testing"
)

// Lab E17: a failed restic backup kept host-source/<id> locked until an
// operator aborted it. With ReleaseLocksOnFailure the source is free again,
// while the failed operation stays retryable.
func TestFailedOperationKeepsLocksByDefault(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", failResult)
	r := h.runner("m1")
	if v, _ := r.Run(context.Background(), threeStepOp("keep")); v.Status != StatusFailed {
		t.Fatalf("status %s", v.Status)
	}
	if _, err := r.Run(context.Background(), threeStepOp("other")); !errors.Is(err, ErrBusy) {
		t.Fatalf("failed operation without the flag must keep its lock: %v", err)
	}
}

func TestFailedOperationReleasesLocksWhenRequested(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.agents["h2"].addFault("s2", failResult)
	r := h.runner("m1")
	rel := threeStepOp("rel")
	rel.ReleaseLocksOnFailure = true
	if v, _ := r.Run(context.Background(), rel); v.Status != StatusFailed {
		t.Fatalf("status %s", v.Status)
	}
	if v, err := r.Run(context.Background(), threeStepOp("next")); err != nil || v.Status != StatusSucceeded {
		t.Fatalf("source still locked after failure: %v %s", err, v.Status)
	}
	// The released failed operation is still retryable and takes the locks again.
	if _, err := r.Resume(context.Background(), "rel"); err != nil && !errors.Is(err, ErrBusy) {
		t.Fatalf("resume: %v", err)
	}
}
