// SPDX-License-Identifier: AGPL-3.0-only

package opjournal

import (
	"errors"
	"reflect"
	"testing"
)

func newOp(t *testing.T, id string, steps []string, term uint64) *Journal {
	t.Helper()
	j := New()
	if err := j.Begin(id, "backup", steps, term); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return j
}

func stepStates(t *testing.T, j *Journal, id string) []State {
	t.Helper()
	op, err := j.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []State
	for _, s := range op.Steps {
		out = append(out, s.State)
	}
	return out
}

func TestBegin(t *testing.T) {
	j := New()
	if err := j.Begin("op1", "backup", []string{"a", "b"}, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.Begin("op1", "backup", []string{"a"}, 1); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Begin = %v, want ErrExists", err)
	}
	op, err := j.Get("op1")
	if err != nil {
		t.Fatal(err)
	}
	want := Operation{ID: "op1", Kind: "backup", Owner: 1, Steps: []Step{{ID: "a", State: Pending}, {ID: "b", State: Pending}}}
	if !reflect.DeepEqual(op, want) {
		t.Fatalf("got %+v, want %+v", op, want)
	}
	if _, err := j.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestOrderedExecution(t *testing.T) {
	j := newOp(t, "op", []string{"a", "b"}, 1)
	if err := j.StartStep("op", "b", 1); err == nil {
		t.Fatal("starting step 2 before step 1 succeeded must fail")
	}
	if err := j.StartStep("op", "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.StartStep("op", "b", 1); err == nil {
		t.Fatal("step 2 must not start while step 1 is Running")
	}
	if err := j.FinishStep("op", "a", Succeeded, 1); err != nil {
		t.Fatal(err)
	}
	if err := j.StartStep("op", "b", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.StartStep("op", "b", 1); err == nil {
		t.Fatal("starting an already Running step must fail")
	}
	if err := j.StartStep("op", "zzz", 1); err == nil {
		t.Fatal("unknown step must fail")
	}
	if err := j.StartStep("nope", "a", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestFinishStepValidation(t *testing.T) {
	j := newOp(t, "op", []string{"a"}, 1)
	if err := j.FinishStep("op", "a", Succeeded, 1); err == nil {
		t.Fatal("finishing a non-Running step must fail")
	}
	if err := j.StartStep("op", "a", 1); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []State{Pending, Running, Aborted, "bogus"} {
		if err := j.FinishStep("op", "a", bad, 1); err == nil {
			t.Fatalf("result %q must be rejected", bad)
		}
	}
	if err := j.FinishStep("op", "a", Failed, 1); err != nil {
		t.Fatal(err)
	}
	// A Failed step can be retried.
	if err := j.StartStep("op", "a", 1); err != nil {
		t.Fatal(err)
	}
	op, _ := j.Get("op")
	if op.Steps[0].Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", op.Steps[0].Attempts)
	}
}

func TestStaleTermIsNotOwner(t *testing.T) {
	j := newOp(t, "op", []string{"a"}, 2)
	if err := j.StartStep("op", "a", 1); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("StartStep stale = %v", err)
	}
	if err := j.StartStep("op", "a", 3); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("StartStep future = %v", err)
	}
	if err := j.StartStep("op", "a", 2); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishStep("op", "a", Succeeded, 1); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("FinishStep stale = %v", err)
	}
	if err := j.Abort("op", 1); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Abort stale = %v", err)
	}
}

func TestAdopt(t *testing.T) {
	j := newOp(t, "op", []string{"a", "b"}, 1)
	_ = j.StartStep("op", "a", 1)
	_ = j.FinishStep("op", "a", Succeeded, 1)
	_ = j.StartStep("op", "b", 1)

	if err := j.Adopt("op", 0); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Adopt lower = %v", err)
	}
	if err := j.Adopt("nope", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Adopt missing = %v", err)
	}
	if err := j.Adopt("op", 5); err != nil {
		t.Fatal(err)
	}
	op, _ := j.Get("op")
	if op.Owner != 5 {
		t.Fatalf("Owner = %d", op.Owner)
	}
	if got, want := stepStates(t, j, "op"), []State{Succeeded, Unknown}; !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	// Old owner is fenced out.
	if err := j.FinishStep("op", "b", Succeeded, 1); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("old owner FinishStep = %v", err)
	}
	// Lower than the new owner is rejected.
	if err := j.Adopt("op", 3); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Adopt lower than owner = %v", err)
	}
}

// Design choice: Adopt with newTerm == Owner is a successful no-op. It must not
// reset steps that the new owner has meanwhile started, so a replayed Adopt
// command is harmless.
func TestAdoptIdempotentSameTerm(t *testing.T) {
	j := newOp(t, "op", []string{"a"}, 1)
	_ = j.StartStep("op", "a", 1)
	if err := j.Adopt("op", 2); err != nil {
		t.Fatal(err)
	}
	if err := j.StartStep("op", "a", 2); err != nil {
		t.Fatal(err)
	}
	if err := j.Adopt("op", 2); err != nil {
		t.Fatalf("repeated Adopt = %v, want nil", err)
	}
	if got, want := stepStates(t, j, "op"), []State{Running}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated Adopt changed states: %v", got)
	}
	op, _ := j.Get("op")
	if op.Owner != 2 || op.Steps[0].Attempts != 2 {
		t.Fatalf("unexpected op %+v", op)
	}
}

func TestUnknownStepRestartedByNewOwner(t *testing.T) {
	j := newOp(t, "op", []string{"a"}, 1)
	_ = j.StartStep("op", "a", 1)
	_ = j.Adopt("op", 2)
	if err := j.StartStep("op", "a", 2); err != nil {
		t.Fatal(err)
	}
	op, _ := j.Get("op")
	if op.Steps[0].State != Running || op.Steps[0].Attempts != 2 {
		t.Fatalf("step = %+v", op.Steps[0])
	}
	// FinishStep may also record Unknown.
	if err := j.FinishStep("op", "a", Unknown, 2); err != nil {
		t.Fatal(err)
	}
}

func TestAbort(t *testing.T) {
	j := newOp(t, "op", []string{"a", "b"}, 1)
	_ = j.StartStep("op", "a", 1)
	if err := j.Abort("op", 1); err == nil {
		t.Fatal("Abort must be refused while a step is Running")
	}
	_ = j.FinishStep("op", "a", Failed, 1)
	if err := j.Abort("op", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.Abort("op", 1); err != nil {
		t.Fatalf("repeated Abort = %v", err)
	}
	if err := j.StartStep("op", "a", 1); err == nil {
		t.Fatal("StartStep on aborted operation must fail")
	}
	if err := j.Abort("nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if len(j.Unfinished()) != 0 {
		t.Fatal("aborted operation must not be unfinished")
	}
}

func TestUnfinished(t *testing.T) {
	j := New()
	for _, id := range []string{"c", "a", "b", "d"} {
		if err := j.Begin(id, "k", []string{"s"}, 1); err != nil {
			t.Fatal(err)
		}
	}
	_ = j.StartStep("b", "s", 1)
	_ = j.FinishStep("b", "s", Succeeded, 1)
	_ = j.Abort("d", 1)
	var ids []string
	for _, op := range j.Unfinished() {
		ids = append(ids, op.ID)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("Unfinished = %v, want %v", ids, want)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	j := newOp(t, "op", []string{"a"}, 1)
	op, _ := j.Get("op")
	op.Steps[0].State = Succeeded
	op.Owner = 99
	again, _ := j.Get("op")
	if again.Steps[0].State != Pending || again.Owner != 1 {
		t.Fatalf("journal mutated via Get copy: %+v", again)
	}
	u := j.Unfinished()
	u[0].Steps[0].State = Failed
	again, _ = j.Get("op")
	if again.Steps[0].State != Pending {
		t.Fatal("journal mutated via Unfinished copy")
	}
}

func TestBeginCopiesStepIDs(t *testing.T) {
	ids := []string{"a"}
	j := New()
	_ = j.Begin("op", "k", ids, 1)
	ids[0] = "x"
	op, _ := j.Get("op")
	if op.Steps[0].ID != "a" {
		t.Fatal("Begin must copy stepIDs")
	}
}
