// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Query filters Log contents. Zero fields match everything. From is inclusive,
// To is exclusive. Limit keeps the newest Limit matches.
type Query struct {
	From, To       time.Time
	ResourcePrefix string
	MinSeverity    Severity
	Kind           string
	Limit          int
}

// Snapshot is the serializable state of a Log.
type Snapshot struct {
	NextSeq uint64  `json:"next_seq"`
	Events  []Event `json:"events"`
}

// Log is a bounded, concurrency-safe in-memory event log. When full, the
// oldest entries are dropped.
type Log struct {
	mu     sync.Mutex
	cap    int
	clock  Clock
	seq    uint64
	events []Event
}

// NewLog creates a Log holding at most capacity events (minimum 1).
func NewLog(capacity int, clock Clock) *Log {
	if capacity < 1 {
		capacity = 1
	}
	if clock == nil {
		clock = time.Now
	}
	return &Log{cap: capacity, clock: clock}
}

// Append stores e, assigning ID and Time when empty, and returns the stored copy.
func (l *Log) Append(e Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	if e.ID == "" {
		e.ID = fmt.Sprintf("ev-%d", l.seq)
	}
	if e.Time.IsZero() {
		e.Time = l.clock()
	}
	e = e.clone()
	l.events = append(l.events, e)
	l.trim()
	return e.clone()
}

func (l *Log) trim() {
	if over := len(l.events) - l.cap; over > 0 {
		l.events = append([]Event(nil), l.events[over:]...)
	}
}

// Len returns the number of stored events.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// Query returns matching events ordered by time, ties by insertion order.
func (l *Log) Query(q Query) []Event {
	l.mu.Lock()
	var out []Event
	for _, e := range l.events {
		if !q.From.IsZero() && e.Time.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !e.Time.Before(q.To) {
			continue
		}
		if q.ResourcePrefix != "" && !strings.HasPrefix(e.Resource, q.ResourcePrefix) {
			continue
		}
		if q.MinSeverity != SeverityUnknown && e.Severity.rank() < q.MinSeverity.rank() {
			continue
		}
		if q.Kind != "" && e.Kind != q.Kind {
			continue
		}
		out = append(out, e.clone())
	}
	l.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out
}

// Snapshot returns a deep copy of the log state.
func (l *Log) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Snapshot{NextSeq: l.seq}
	for _, e := range l.events {
		s.Events = append(s.Events, e.clone())
	}
	return s
}

// Restore replaces the log contents, keeping only the newest events that fit.
func (l *Log) Restore(s Snapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = nil
	l.seq = s.NextSeq
	for _, e := range s.Events {
		l.events = append(l.events, e.clone())
		if n, err := strconv.ParseUint(strings.TrimPrefix(e.ID, "ev-"), 10, 64); err == nil && n > l.seq {
			l.seq = n
		}
	}
	l.trim()
}
