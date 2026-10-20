// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ms(v ...int) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, x := range v {
		out[i] = time.Duration(x) * time.Millisecond
	}
	return out
}

func node(id string, lat ...int) Node {
	return Node{ID: id, Healthy: true, LastSeen: t0, Role: RoleWriterEligible, Sync: SyncSynced, LatencySamples: ms(lat...)}
}

func TestEWMA(t *testing.T) {
	tests := []struct {
		name   string
		in     []time.Duration
		alpha  float64
		want   time.Duration
		wantOK bool
	}{
		{"empty", nil, 0.5, 0, false},
		{"ignores non-positive", ms(0, -5), 0.5, 0, false},
		{"single", ms(10), 0.5, 10 * time.Millisecond, true},
		{"two", ms(10, 20), 0.5, 15 * time.Millisecond, true},
		{"three", ms(10, 20, 40), 0.5, 27500 * time.Microsecond, true},
		{"alpha one is last", ms(10, 20, 40), 1, 40 * time.Millisecond, true},
	}
	for _, tt := range tests {
		got, ok := EWMA(tt.in, tt.alpha)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("%s: got %v,%v want %v,%v", tt.name, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestDefaults(t *testing.T) {
	p := DefaultPolicy()
	if p.StaleAfter != DefaultStaleAfter || p.EWMAAlpha != DefaultEWMAAlpha ||
		p.SwitchMargin != DefaultSwitchMargin || p.SwitchConsecutive != DefaultSwitchConsecutive ||
		p.DrainDeadline != DefaultDrainDeadline {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	q := Policy{EWMAAlpha: 7, SwitchMargin: -1}.withDefaults()
	if q.EWMAAlpha != DefaultEWMAAlpha || q.SwitchMargin != DefaultSwitchMargin {
		t.Fatalf("invalid values not replaced: %+v", q)
	}
}

func TestEligibility(t *testing.T) {
	mut := func(f func(*Node)) Node { n := node("a", 5); f(&n); return n }
	tests := []struct {
		name string
		n    Node
		want bool
	}{
		{"healthy", node("a", 5), true},
		{"unhealthy", mut(func(n *Node) { n.Healthy = false }), false},
		{"never seen", mut(func(n *Node) { n.LastSeen = time.Time{} }), false},
		{"stale", mut(func(n *Node) { n.LastSeen = t0.Add(-DefaultStaleAfter - time.Second) }), false},
		{"exactly stale boundary", mut(func(n *Node) { n.LastSeen = t0.Add(-DefaultStaleAfter) }), true},
		{"far future", mut(func(n *Node) { n.LastSeen = t0.Add(time.Hour) }), false},
		{"unsynced zero", mut(func(n *Node) { n.Sync = SyncUnknown }), false},
		{"joining", mut(func(n *Node) { n.Sync = SyncJoining }), false},
		{"donor", mut(func(n *Node) { n.Sync = SyncDonor }), false},
		{"desynced", mut(func(n *Node) { n.Sync = SyncDesynced }), false},
		{"standby", mut(func(n *Node) { n.Role = RoleStandby }), false},
		{"excluded", mut(func(n *Node) { n.Role = RoleExcluded }), false},
		{"zero role fails closed", mut(func(n *Node) { n.Role = 0 }), false},
		{"reader can't write", mut(func(n *Node) { n.Role = RoleReader }), false},
	}
	for _, tt := range tests {
		d := New(Policy{}).Decide(t0, []Node{tt.n})
		if d.HasWriter != tt.want {
			t.Errorf("%s: HasWriter=%v want %v (%v)", tt.name, d.HasWriter, tt.want, d.Reasons)
		}
	}
}

func TestNoWriterExplicit(t *testing.T) {
	r := New(Policy{})
	d := r.Decide(t0, []Node{node("a", 5), node("b", 6)})
	if !d.HasWriter {
		t.Fatal("expected writer")
	}
	bad := []Node{node("a", 5), node("b", 6)}
	bad[0].Healthy, bad[1].LastSeen = false, t0.Add(-time.Hour)
	d = r.Decide(t0, bad)
	if d.HasWriter || d.Writer != "" || !d.Changed || d.Previous == "" || d.Drain == nil || d.Drain.NodeID != d.Previous {
		t.Fatalf("expected explicit no-writer with drain: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Reasons, ";"), "no writer") {
		t.Fatalf("reason missing: %v", d.Reasons)
	}
	// Recovery picks immediately, with nothing to drain.
	d = r.Decide(t0, []Node{node("b", 6)})
	if d.Writer != "b" || d.Drain != nil {
		t.Fatalf("recovery: %+v", d)
	}
	if d := New(Policy{}).Decide(t0, nil); d.HasWriter {
		t.Fatal("empty input must not have a writer")
	}
}

func TestStableKeepsWriterAndFailsOverImmediately(t *testing.T) {
	r := New(Policy{})
	d := r.Decide(t0, []Node{node("a", 10), node("b", 20)})
	if d.Writer != "a" || !d.Changed || d.Drain != nil {
		t.Fatalf("initial: %+v", d)
	}
	for i := 0; i < 10; i++ {
		d = r.Decide(t0, []Node{node("a", 100), node("b", 1)})
		if d.Writer != "a" || d.Changed {
			t.Fatalf("round %d: %+v", i, d)
		}
	}
	na := node("a", 100)
	na.Healthy = false
	now := t0.Add(time.Second)
	nb := node("b", 1)
	nb.LastSeen = now
	d = r.Decide(now, []Node{na, nb})
	if d.Writer != "b" || !d.Changed || d.Previous != "a" {
		t.Fatalf("failover: %+v", d)
	}
	if d.Drain == nil || d.Drain.NodeID != "a" || d.Drain.AcceptNew || !d.Drain.Deadline.Equal(now.Add(DefaultDrainDeadline)) {
		t.Fatalf("drain: %+v", d.Drain)
	}
}

func TestStaleWriterReplacedImmediately(t *testing.T) {
	r := New(Policy{})
	r.Decide(t0, []Node{node("a", 1), node("b", 50)})
	later := t0.Add(DefaultStaleAfter + time.Second)
	a, b := node("a", 1), node("b", 50)
	b.LastSeen = later
	d := r.Decide(later, []Node{a, b})
	if d.Writer != "b" {
		t.Fatalf("stale writer must be replaced: %+v", d)
	}
}

func TestLowestLatencyHysteresis(t *testing.T) {
	p := Policy{Mode: WriterLowestLatency, SwitchConsecutive: 3, SwitchMargin: 0.2}
	step := func(r *Router, a, b int) Decision { return r.Decide(t0, []Node{node("a", a), node("b", b)}) }

	t.Run("switch after N consecutive", func(t *testing.T) {
		r := New(p)
		step(r, 10, 12)
		for i, want := range []string{"a", "a", "b"} {
			d := step(r, 10, 5)
			if d.Writer != want {
				t.Fatalf("eval %d: %s want %s: %v", i, d.Writer, want, d.Reasons)
			}
			if want == "b" && (d.Drain == nil || d.Drain.NodeID != "a") {
				t.Fatalf("missing drain: %+v", d)
			}
		}
	})
	t.Run("margin not met", func(t *testing.T) {
		r := New(p)
		step(r, 10, 12)
		for i := 0; i < 20; i++ {
			if d := step(r, 10, 9); d.Writer != "a" {
				t.Fatalf("switched within margin: %+v", d)
			}
		}
	})
	t.Run("streak resets on interruption", func(t *testing.T) {
		r := New(p)
		step(r, 10, 12)
		step(r, 10, 5)
		step(r, 10, 5)
		step(r, 10, 9)
		if d := step(r, 10, 5); d.Writer != "a" {
			t.Fatalf("streak not reset: %+v", d)
		}
	})
	t.Run("streak resets on candidate change", func(t *testing.T) {
		r := New(p)
		r.Decide(t0, []Node{node("a", 10), node("b", 12), node("c", 12)})
		r.Decide(t0, []Node{node("a", 10), node("b", 5), node("c", 12)})
		r.Decide(t0, []Node{node("a", 10), node("b", 5), node("c", 12)})
		d := r.Decide(t0, []Node{node("a", 10), node("b", 12), node("c", 4)})
		if d.Writer != "a" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("unknown latency never wins switch", func(t *testing.T) {
		r := New(p)
		step(r, 10, 12)
		for i := 0; i < 5; i++ {
			d := r.Decide(t0, []Node{node("a", 10), node("b")})
			if d.Writer != "a" {
				t.Fatalf("%+v", d)
			}
		}
	})
}

func TestPreferNode(t *testing.T) {
	r := New(Policy{Mode: WriterPreferNode, PreferredID: "b", SwitchConsecutive: 2})
	nb := node("b", 50)
	nb.Healthy = false
	d := r.Decide(t0, []Node{node("a", 10), nb})
	if d.Writer != "a" {
		t.Fatalf("%+v", d)
	}
	if d = r.Decide(t0, []Node{node("a", 10), node("b", 50)}); d.Writer != "a" {
		t.Fatalf("%+v", d)
	}
	if d = r.Decide(t0, []Node{node("a", 10), node("b", 50)}); d.Writer != "b" || d.Drain == nil {
		t.Fatalf("%+v", d)
	}
	for i := 0; i < 5; i++ {
		if d = r.Decide(t0, []Node{node("a", 1), node("b", 50)}); d.Writer != "b" {
			t.Fatalf("%+v", d)
		}
	}
	nb = node("b", 50)
	nb.Sync = SyncDesynced
	if d = r.Decide(t0, []Node{node("a", 1), nb}); d.Writer != "a" || !d.Changed {
		t.Fatalf("%+v", d)
	}
	r2 := New(Policy{Mode: WriterPreferNode, PreferredID: "b"})
	if d = r2.Decide(t0, []Node{node("a", 1), node("b", 50)}); d.Writer != "b" {
		t.Fatalf("%+v", d)
	}
}

func TestNeverSelectsIneligible(t *testing.T) {
	sb, ex, un := node("sb", 1), node("ex", 1), node("un", 1)
	sb.Role, ex.Role, un.Sync = RoleStandby, RoleExcluded, SyncJoining
	for _, mode := range []WriterMode{WriterStable, WriterLowestLatency, WriterPreferNode} {
		for _, pref := range []string{"sb", "ex", "un", "ok"} {
			r := New(Policy{Mode: mode, PreferredID: pref, Read: ReadIncludeWriter})
			for i := 0; i < 5; i++ {
				d := r.Decide(t0, []Node{sb, ex, un, node("ok", 100)})
				if d.Writer != "ok" || !reflect.DeepEqual(d.Readers, []string{"ok"}) {
					t.Fatalf("mode %v pref %s: %+v", mode, pref, d)
				}
			}
		}
	}
}

func TestReaders(t *testing.T) {
	rd := node("r", 5)
	rd.Role = RoleReader
	rd2 := node("r2", 9)
	rd2.Role = RoleReader
	sb := node("s", 1)
	sb.Role = RoleStandby
	w := node("w", 20)
	tests := []struct {
		name  string
		mode  ReadMode
		nodes []Node
		want  []string
	}{
		{"disabled", ReadDisabled, []Node{w, rd}, nil},
		{"exclude writer", ReadExcludeWriter, []Node{w, rd2, rd, sb}, []string{"r", "r2"}},
		{"include writer ordered", ReadIncludeWriter, []Node{w, rd2, rd, sb}, []string{"r", "r2", "w"}},
		{"exclude, none left", ReadExcludeWriter, []Node{w}, nil},
		{"fallback to writer", ReadFallbackToWriter, []Node{w}, []string{"w"}},
		{"fallback not used", ReadFallbackToWriter, []Node{w, rd}, []string{"r"}},
		{"no writer, readers still listed", ReadExcludeWriter, []Node{rd}, []string{"r"}},
	}
	for _, tt := range tests {
		d := New(Policy{Read: tt.mode}).Decide(t0, tt.nodes)
		if !reflect.DeepEqual(d.Readers, tt.want) {
			t.Errorf("%s: got %v want %v (%v)", tt.name, d.Readers, tt.want, d.Reasons)
		}
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := New(Policy{}).Decide(t0, []Node{node("c", 5), node("b", 5), node("a", 5)})
		if d.Writer != "a" {
			t.Fatalf("tie must resolve by ID: %+v", d)
		}
	}
}

// Property: jittery but statistically equal latencies must not flap.
func TestNoFlappingUnderJitter(t *testing.T) {
	for _, mode := range []WriterMode{WriterStable, WriterLowestLatency} {
		for seed := int64(1); seed <= 20; seed++ {
			rng := rand.New(rand.NewSource(seed))
			r := New(Policy{Mode: mode})
			hist := map[string][]time.Duration{}
			changes := 0
			for i := 0; i < 500; i++ {
				var nodes []Node
				for _, id := range []string{"a", "b", "c"} {
					s := time.Duration(10 * (1 + (rng.Float64()-0.5)*0.4) * float64(time.Millisecond)) // +-20%
					hist[id] = append(hist[id], s)
					if len(hist[id]) > 8 {
						hist[id] = hist[id][1:]
					}
					n := node(id)
					n.LatencySamples = append([]time.Duration(nil), hist[id]...)
					nodes = append(nodes, n)
				}
				if d := r.Decide(t0, nodes); d.Changed && i > 0 {
					changes++
				}
			}
			if changes != 0 {
				t.Fatalf("mode %v seed %d: writer flapped %d times", mode, seed, changes)
			}
		}
	}
}

// Property: a failed writer is replaced in the very same evaluation, for every
// mode and every way of failing, regardless of hysteresis state.
func TestFailoverNeverDelayed(t *testing.T) {
	fails := map[string]func(*Node){
		"unhealthy": func(n *Node) { n.Healthy = false },
		"stale":     func(n *Node) { n.LastSeen = t0.Add(-time.Hour) },
		"unsynced":  func(n *Node) { n.Sync = SyncDesynced },
		"excluded":  func(n *Node) { n.Role = RoleExcluded },
		"standby":   func(n *Node) { n.Role = RoleStandby },
	}
	for _, mode := range []WriterMode{WriterStable, WriterLowestLatency, WriterPreferNode} {
		for name, fail := range fails {
			for streak := 0; streak < 3; streak++ {
				r := New(Policy{Mode: mode, PreferredID: "a"})
				r.Decide(t0, []Node{node("a", 10), node("b", 12)})
				for i := 0; i < streak; i++ {
					r.Decide(t0, []Node{node("a", 10), node("b", 1)})
				}
				w := r.Current()
				var nodes []Node
				for _, id := range []string{"a", "b"} {
					n := node(id, 10)
					if id == w {
						fail(&n)
					}
					nodes = append(nodes, n)
				}
				d := r.Decide(t0, nodes)
				other := map[string]string{"a": "b", "b": "a"}[w]
				if d.Writer != other || !d.Changed || d.Drain == nil || d.Drain.NodeID != w {
					t.Fatalf("mode %v fail %s streak %d: %+v", mode, name, streak, d)
				}
			}
		}
	}
}

// Property: when nothing is usable, the answer is "no writer", for any mix of
// failure modes.
func TestAllUnhealthyNoWriter(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		var nodes []Node
		for j := 0; j < 1+rng.Intn(5); j++ {
			n := node(string(rune('a'+j)), 1+rng.Intn(50))
			switch rng.Intn(5) {
			case 0:
				n.Healthy = false
			case 1:
				n.LastSeen = time.Time{}
			case 2:
				n.LastSeen = t0.Add(-time.Minute)
			case 3:
				n.Sync = SyncUnknown
			case 4:
				n.Role = RoleStandby
			}
			nodes = append(nodes, n)
		}
		d := New(Policy{Read: ReadIncludeWriter}).Decide(t0, nodes)
		if d.HasWriter || d.Writer != "" || len(d.Readers) != 0 {
			t.Fatalf("iteration %d: %+v", i, d)
		}
	}
}

func TestSetPolicyKeepsWriterAndResetsStreak(t *testing.T) {
	r := New(Policy{Mode: WriterLowestLatency, SwitchConsecutive: 3})
	nodes := []Node{node("a", 10), node("b", 20)}
	if d := r.Decide(t0, nodes); d.Writer != "a" {
		t.Fatalf("writer = %q, want a", d.Writer)
	}
	// b becomes much faster: one qualifying evaluation builds a streak.
	nodes = []Node{node("a", 50), node("b", 5)}
	r.Decide(t0, nodes)

	r.SetPolicy(Policy{Mode: WriterLowestLatency, SwitchConsecutive: 2, DrainDeadline: time.Second})
	if r.Current() != "a" {
		t.Fatalf("SetPolicy changed current writer to %q", r.Current())
	}
	if got := r.Policy().DrainDeadline; got != time.Second {
		t.Fatalf("DrainDeadline = %s, want 1s", got)
	}
	if got := r.Policy().StaleAfter; got != DefaultStaleAfter {
		t.Fatalf("defaults not applied: StaleAfter = %s", got)
	}
	// The streak was reset, so one evaluation is not enough for 2 consecutive.
	if d := r.Decide(t0, nodes); d.Writer != "a" || d.Changed {
		t.Fatalf("switched after reset streak: %+v", d)
	}
	if d := r.Decide(t0, nodes); d.Writer != "b" || !d.Changed || d.Drain == nil || d.Drain.NodeID != "a" {
		t.Fatalf("expected switch to b with drain of a: %+v", d)
	}
}
