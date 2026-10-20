// SPDX-License-Identifier: AGPL-3.0-only

package opjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// populated returns a journal that covers every entry shape a snapshot must
// keep: pending, running, succeeded, unknown after adoption, and aborted.
func populated(t *testing.T) *Journal {
	t.Helper()
	j := New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(j.Begin("op1", "failover", []string{"a", "b"}, 3))
	must(j.StartStep("op1", "a", 3))
	must(j.FinishStep("op1", "a", Succeeded, 3))
	must(j.StartStep("op1", "b", 3))
	must(j.Adopt("op1", 5)) // b becomes Unknown
	must(j.Begin("op2", "backup", []string{"x"}, 3))
	must(j.Abort("op2", 3))
	must(j.Begin("op3", "backup", []string{"y"}, 4))
	must(j.StartStep("op3", "y", 4))
	must(j.FinishStep("op3", "y", Succeeded, 4)) // finished
	must(j.Begin("op4", "restore", nil, 4))      // no steps
	return j
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	src := populated(t)
	data, err := src.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	dst, err := Restore(data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, id := range []string{"op1", "op2", "op3", "op4"} {
		a, errA := src.Get(id)
		b, errB := dst.Get(id)
		if errA != nil || errB != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: src %+v (%v), restored %+v (%v)", id, a, errA, b, errB)
		}
	}
	if !reflect.DeepEqual(src.Unfinished(), dst.Unfinished()) {
		t.Fatalf("Unfinished differs: src %+v, restored %+v", src.Unfinished(), dst.Unfinished())
	}
	// The aborted flag survives: op2 is aborted, so starting its step fails.
	if err := dst.StartStep("op2", "x", 3); err == nil {
		t.Fatal("StartStep on restored aborted operation: got nil error")
	}
	// The restored journal keeps working under the same rules.
	if err := dst.StartStep("op1", "b", 5); err != nil {
		t.Fatalf("StartStep on restored journal: %v", err)
	}
	if err := dst.StartStep("op1", "b", 3); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("old term on restored journal: got %v, want ErrNotOwner", err)
	}
}

func TestSnapshotIsDeterministic(t *testing.T) {
	a, err := populated(t).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := populated(t).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("snapshots differ:\n%s\n%s", a, b)
	}
}

// The snapshot holds current state only: repeating commands that leave the
// state the same does not make it grow.
func TestSnapshotSizeTracksStateNotHistory(t *testing.T) {
	j := New()
	if err := j.Begin("op1", "k", []string{"a"}, 1); err != nil {
		t.Fatal(err)
	}
	before, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for term := uint64(2); term < 200; term++ {
		if err := j.Adopt("op1", term); err != nil {
			t.Fatal(err)
		}
	}
	after, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Only the owner number changes ("1" -> "199").
	if len(after)-len(before) > 2 {
		t.Fatalf("snapshot grew from %d to %d bytes after 198 adopts", len(before), len(after))
	}
}

func TestSnapshotIsIndependentOfLaterChanges(t *testing.T) {
	j := populated(t)
	data, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.StartStep("op1", "b", 5); err != nil {
		t.Fatal(err)
	}
	r, err := Restore(data)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := r.Get("op1")
	if op.Steps[1].State != Unknown {
		t.Fatalf("restored step state %s, want unknown", op.Steps[1].State)
	}
}

func TestRestoreEmptyJournal(t *testing.T) {
	data, err := New().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	j, err := Restore(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Unfinished(); len(got) != 0 {
		t.Fatalf("Unfinished = %+v, want empty", got)
	}
}

func TestRestoreRejectsInvalidInput(t *testing.T) {
	valid := func(mut func(*snapshotDoc)) []byte {
		doc := snapshotDoc{Version: snapshotVersion, Operations: []snapshotOp{
			{Operation: Operation{ID: "op1", Kind: "k", Owner: 1, Steps: []Step{{ID: "a", State: Pending}}}},
		}}
		mut(&doc)
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := map[string][]byte{
		"not json":       []byte("{"),
		"wrong version":  valid(func(d *snapshotDoc) { d.Version = 99 }),
		"duplicate id":   valid(func(d *snapshotDoc) { d.Operations = append(d.Operations, d.Operations[0]) }),
		"empty id":       valid(func(d *snapshotDoc) { d.Operations[0].ID = "" }),
		"invalid state":  valid(func(d *snapshotDoc) { d.Operations[0].Steps[0].State = "bogus" }),
		"negative tries": valid(func(d *snapshotDoc) { d.Operations[0].Steps[0].Attempts = -1 }),
		"trailing data":  append(valid(func(*snapshotDoc) {}), []byte(" {}")...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Restore(data); err == nil {
				t.Fatalf("Restore(%s): got nil error", strings.TrimSpace(string(data)))
			}
		})
	}
	if _, err := Restore(valid(func(*snapshotDoc) {})); err != nil {
		t.Fatalf("Restore(valid): %v", err)
	}
}
