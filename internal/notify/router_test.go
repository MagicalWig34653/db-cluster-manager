// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newRouter(c *fakeClock, rules ...Rule) *Router {
	return NewRouter(rules, c.Now)
}

func TestMatcher(t *testing.T) {
	m := Matcher{Kinds: []string{"node.down", "backup.*"}, ResourcePrefix: "cluster/a", MinSeverity: SeverityWarning}
	cases := []struct {
		e    Event
		want bool
	}{
		{Event{Kind: "node.down", Resource: "cluster/a/n1", Severity: SeverityError}, true},
		{Event{Kind: "backup.failed", Resource: "cluster/a", Severity: SeverityWarning}, true},
		{Event{Kind: "node.up", Resource: "cluster/a", Severity: SeverityError}, false},
		{Event{Kind: "node.down", Resource: "cluster/b", Severity: SeverityError}, false},
		{Event{Kind: "node.down", Resource: "cluster/a", Severity: SeverityInfo}, false},
		{Event{Kind: "node.down", Resource: "cluster/a", Severity: SeverityUnknown}, true},
	}
	for i, c := range cases {
		if got := m.Matches(c.e); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
	if !(Matcher{}).Matches(Event{}) {
		t.Fatal("empty matcher matches all")
	}
}

func TestCooldownPerGroup(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui", "mail"}, Cooldown: 10 * time.Minute, GroupBy: []string{"kind", "resource"}})
	ev := Event{Kind: "k", Resource: "a"}

	d, _ := r.Route(ev)
	if len(d) != 2 {
		t.Fatalf("first event delivers to both channels, got %d", len(d))
	}
	c.Advance(time.Minute)
	d, s := r.Route(ev)
	if len(d) != 0 || len(s) != 1 || s[0].Reason != ReasonCooldown {
		t.Fatalf("expected cooldown suppression: %+v %+v", d, s)
	}
	// different group is independent
	if d, _ := r.Route(Event{Kind: "k", Resource: "b"}); len(d) != 2 {
		t.Fatal("other group must deliver")
	}
	c.Advance(10 * time.Minute)
	d, _ = r.Route(ev)
	if len(d) != 2 || d[0].Suppressed != 1 {
		t.Fatalf("after cooldown deliver and report suppressed count: %+v", d)
	}
}

func TestGroupByDetail(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}, Cooldown: time.Hour, GroupBy: []string{"detail:db"}})
	r.Route(Event{Details: map[string]string{"db": "x"}})
	if d, _ := r.Route(Event{Details: map[string]string{"db": "y"}}); len(d) != 1 {
		t.Fatal("different detail value is a different group")
	}
	if d, _ := r.Route(Event{Details: map[string]string{"db": "x"}}); len(d) != 0 {
		t.Fatal("same detail value is deduplicated")
	}
}

func TestNoCooldownDeliversEvery(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}})
	for i := 0; i < 3; i++ {
		if d, _ := r.Route(Event{}); len(d) != 1 {
			t.Fatal("no cooldown means every event delivers")
		}
	}
}

func TestFlappingSuppression(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}, FlapWindow: 10 * time.Minute, FlapMax: 3})
	ev := Event{Kind: "k"}
	delivered := 0
	for i := 0; i < 6; i++ {
		d, s := r.Route(ev)
		delivered += len(d)
		if i >= 3 && (len(s) != 1 || s[0].Reason != ReasonFlapping) {
			t.Fatalf("event %d should be flapping-suppressed: %+v", i, s)
		}
		c.Advance(time.Minute)
	}
	if delivered != 3 {
		t.Fatalf("delivered %d", delivered)
	}
	// quiet for a full window -> recovers
	c.Advance(11 * time.Minute)
	if d, _ := r.Route(ev); len(d) != 1 {
		t.Fatal("flapping must end after a quiet window")
	}
}

func TestEscalation(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}, EscalateTo: "pager", EscalateAfter: 15 * time.Minute})
	d, _ := r.Route(Event{Kind: "k"})
	key := d[0].Key
	c.Advance(14 * time.Minute)
	if got := r.Due(); len(got) != 0 {
		t.Fatal("too early")
	}
	c.Advance(time.Minute)
	got := r.Due()
	if len(got) != 1 || got[0].Channel != "pager" || !got[0].Escalated || got[0].Key != key {
		t.Fatalf("due: %+v", got)
	}
	if len(r.Due()) != 0 {
		t.Fatal("escalate only once")
	}
}

func TestEscalationCancelledByAck(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}, EscalateTo: "pager", EscalateAfter: time.Minute})
	d, _ := r.Route(Event{})
	if !r.Ack(d[0].Key) {
		t.Fatal("ack of pending key returns true")
	}
	c.Advance(time.Hour)
	if len(r.Due()) != 0 {
		t.Fatal("acked notification must not escalate")
	}
	if r.Ack("nope") {
		t.Fatal("unknown key")
	}
}

func TestSuppressedRepeatDoesNotResetEscalation(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c, Rule{Name: "r", Channels: []string{"ui"}, Cooldown: time.Hour, EscalateTo: "pager", EscalateAfter: 5 * time.Minute})
	r.Route(Event{})
	c.Advance(4 * time.Minute)
	r.Route(Event{}) // suppressed
	c.Advance(time.Minute)
	if len(r.Due()) != 1 {
		t.Fatal("escalation timer runs from the first delivery")
	}
}

func TestEventMatchingMultipleRules(t *testing.T) {
	c := &fakeClock{now: t0}
	r := newRouter(c,
		Rule{Name: "a", Channels: []string{"ui"}},
		Rule{Name: "b", Match: Matcher{MinSeverity: SeverityCritical}, Channels: []string{"mail"}})
	d, _ := r.Route(Event{Severity: SeverityCritical})
	if len(d) != 2 {
		t.Fatalf("got %d", len(d))
	}
	d, _ = r.Route(Event{Severity: SeverityInfo})
	if len(d) != 1 || d[0].Channel != "ui" {
		t.Fatalf("got %+v", d)
	}
}
