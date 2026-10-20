// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestSeverityUnknownIsNotHealthy(t *testing.T) {
	var s Severity
	if s != SeverityUnknown {
		t.Fatalf("zero severity must be unknown, got %v", s)
	}
	if s.String() != "unknown" {
		t.Fatalf("string = %q", s.String())
	}
	if s.rank() < SeverityWarning.rank() {
		t.Fatal("unknown must rank at least as high as warning")
	}
	if s.rank() <= SeverityInfo.rank() {
		t.Fatal("unknown must not look like info")
	}
}

func TestLogAppendAssignsIDAndTime(t *testing.T) {
	l := NewLog(10, func() time.Time { return t0 })
	e := l.Append(Event{Kind: "k", Resource: "cluster/a"})
	if e.ID == "" || !e.Time.Equal(t0) {
		t.Fatalf("id/time not assigned: %+v", e)
	}
	e2 := l.Append(Event{Kind: "k"})
	if e2.ID == e.ID {
		t.Fatal("ids must be unique")
	}
}

func TestLogBounded(t *testing.T) {
	l := NewLog(3, func() time.Time { return t0 })
	for i := 0; i < 5; i++ {
		l.Append(Event{Kind: "k", Message: string(rune('a' + i))})
	}
	got := l.Query(Query{})
	if len(got) != 3 || got[0].Message != "c" || got[2].Message != "e" {
		t.Fatalf("got %+v", got)
	}
	if l.Len() != 3 {
		t.Fatalf("len %d", l.Len())
	}
}

func TestLogQueryFilters(t *testing.T) {
	now := t0
	l := NewLog(10, func() time.Time { return now })
	add := func(res string, sev Severity) {
		l.Append(Event{Resource: res, Severity: sev, Kind: "k"})
		now = now.Add(time.Minute)
	}
	add("cluster/a", SeverityInfo)
	add("cluster/a/node/1", SeverityError)
	add("cluster/b", SeverityCritical)
	add("cluster/a", SeverityWarning)

	if got := l.Query(Query{ResourcePrefix: "cluster/a"}); len(got) != 3 {
		t.Fatalf("resource filter: %d", len(got))
	}
	if got := l.Query(Query{MinSeverity: SeverityError}); len(got) != 2 {
		t.Fatalf("severity filter: %d", len(got))
	}
	if got := l.Query(Query{From: t0.Add(time.Minute), To: t0.Add(3 * time.Minute)}); len(got) != 2 {
		t.Fatalf("time filter [from,to): %d", len(got))
	}
	if got := l.Query(Query{Limit: 2}); len(got) != 2 || got[0].Resource != "cluster/b" || got[1].Resource != "cluster/a" {
		t.Fatalf("limit returns newest N in order: %+v", got)
	}
}

func TestLogStableOrderingByTimeThenInsertion(t *testing.T) {
	l := NewLog(10, func() time.Time { return t0 })
	l.Append(Event{Time: t0.Add(time.Minute), Message: "late"})
	l.Append(Event{Time: t0, Message: "x1"})
	l.Append(Event{Time: t0, Message: "x2"})
	got := l.Query(Query{})
	if got[0].Message != "x1" || got[1].Message != "x2" || got[2].Message != "late" {
		t.Fatalf("order %+v", got)
	}
}

func TestLogSnapshotRestore(t *testing.T) {
	l := NewLog(10, func() time.Time { return t0 })
	a := l.Append(Event{Kind: "k", Details: map[string]string{"a": "1"}})
	snap := l.Snapshot()
	snap.Events[0].Details["a"] = "mutated"
	if l.Query(Query{})[0].Details["a"] != "1" {
		t.Fatal("snapshot must deep copy details")
	}
	l2 := NewLog(10, func() time.Time { return t0 })
	l2.Restore(l.Snapshot())
	if got := l2.Query(Query{}); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("restore %+v", got)
	}
	if b := l2.Append(Event{}); b.ID == a.ID {
		t.Fatal("restored log must not reuse ids")
	}
	small := NewLog(1, func() time.Time { return t0 })
	l.Append(Event{})
	small.Restore(l.Snapshot())
	if small.Len() != 1 {
		t.Fatal("restore must respect capacity")
	}
}

func TestLogQueryReturnsCopies(t *testing.T) {
	l := NewLog(3, func() time.Time { return t0 })
	l.Append(Event{Details: map[string]string{"a": "1"}})
	l.Query(Query{})[0].Details["a"] = "x"
	if l.Query(Query{})[0].Details["a"] != "1" {
		t.Fatal("query must return copies")
	}
}
