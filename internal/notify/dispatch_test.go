// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeChannel struct {
	name string
	mu   sync.Mutex
	got  []Event
	err  error
	pan  bool
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Send(_ context.Context, e Event) error {
	if f.pan {
		panic("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return f.err
}
func (f *fakeChannel) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

func TestFailingChannelDoesNotBlockOthersNorLoseEvent(t *testing.T) {
	c := &fakeClock{now: t0}
	log := NewLog(100, c.Now)
	bad := &fakeChannel{name: "bad", err: errors.New("down")}
	pan := &fakeChannel{name: "pan", pan: true}
	good := &fakeChannel{name: "good"}
	r := NewRouter([]Rule{{Name: "r", Channels: []string{"bad", "pan", "good", "missing"}}}, c.Now)
	d := NewDispatcher(log, r, c.Now, bad, pan, good)

	ev := d.Handle(context.Background(), Event{Kind: "node.down", Resource: "cluster/a", Severity: SeverityError})

	if good.count() != 1 {
		t.Fatal("good channel must still receive the event")
	}
	if got := log.Query(Query{Kind: "node.down"}); len(got) != 1 || got[0].ID != ev.ID {
		t.Fatal("original event must be logged")
	}
	ok := log.Query(Query{Kind: KindDeliveryOK})
	failed := log.Query(Query{Kind: KindDeliveryFailed})
	if len(ok) != 1 || ok[0].Details["channel"] != "good" || ok[0].Details["event_id"] != ev.ID {
		t.Fatalf("ok results: %+v", ok)
	}
	if len(failed) != 3 {
		t.Fatalf("failed results (bad, pan, missing): %+v", failed)
	}
	for _, f := range failed {
		if f.Severity < SeverityWarning || f.Details["error"] == "" {
			t.Fatalf("failure event: %+v", f)
		}
	}
}

func TestDeliveryEventsAreNotRerouted(t *testing.T) {
	c := &fakeClock{now: t0}
	log := NewLog(100, c.Now)
	bad := &fakeChannel{name: "bad", err: errors.New("down")}
	r := NewRouter([]Rule{{Name: "all", Channels: []string{"bad"}}}, c.Now)
	d := NewDispatcher(log, r, c.Now, bad)
	d.Handle(context.Background(), Event{Kind: "x"})
	if bad.count() != 1 {
		t.Fatalf("expected exactly one send, got %d (delivery events re-routed?)", bad.count())
	}
}

func TestSuppressedEventStillLoggedAndCountReported(t *testing.T) {
	c := &fakeClock{now: t0}
	log := NewLog(100, c.Now)
	ch := &fakeChannel{name: "ui"}
	r := NewRouter([]Rule{{Name: "r", Channels: []string{"ui"}, Cooldown: time.Minute}}, c.Now)
	d := NewDispatcher(log, r, c.Now, ch)
	d.Handle(context.Background(), Event{Kind: "k"})
	d.Handle(context.Background(), Event{Kind: "k"})
	if ch.count() != 1 || len(log.Query(Query{Kind: "k"})) != 2 {
		t.Fatal("second event is logged but not delivered")
	}
	if len(log.Query(Query{Kind: KindSuppressed})) != 1 {
		t.Fatal("suppression recorded as event")
	}
	c.Advance(2 * time.Minute)
	d.Handle(context.Background(), Event{Kind: "k"})
	if ch.got[1].Details["suppressed_count"] != "1" {
		t.Fatalf("details: %+v", ch.got[1].Details)
	}
}

func TestEscalationDelivery(t *testing.T) {
	c := &fakeClock{now: t0}
	log := NewLog(100, c.Now)
	ui := &fakeChannel{name: "ui"}
	pager := &fakeChannel{name: "pager"}
	r := NewRouter([]Rule{{Name: "r", Channels: []string{"ui"}, EscalateTo: "pager", EscalateAfter: 5 * time.Minute}}, c.Now)
	d := NewDispatcher(log, r, c.Now, ui, pager)
	d.Handle(context.Background(), Event{Kind: "k", Message: "m", Severity: SeverityError})
	c.Advance(6 * time.Minute)
	d.Escalate(context.Background())
	if pager.count() != 1 || pager.got[0].Details["escalated"] != "true" || pager.got[0].Message != "m" {
		t.Fatalf("pager: %+v", pager.got)
	}
	if len(log.Query(Query{Kind: KindEscalated})) != 1 {
		t.Fatal("escalation recorded")
	}
}

func TestChannelTimeoutBoundsSend(t *testing.T) {
	c := &fakeClock{now: t0}
	log := NewLog(10, c.Now)
	slow := &blockingChannel{}
	r := NewRouter([]Rule{{Name: "r", Channels: []string{"slow"}}}, c.Now)
	d := NewDispatcher(log, r, c.Now, slow)
	d.SendTimeout = 20 * time.Millisecond
	done := make(chan struct{})
	go func() { d.Handle(context.Background(), Event{Kind: "k"}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle must not hang on a blocked channel")
	}
	if len(log.Query(Query{Kind: KindDeliveryFailed})) != 1 {
		t.Fatal("timeout recorded as failure")
	}
}

type blockingChannel struct{}

func (blockingChannel) Name() string { return "slow" }
func (blockingChannel) Send(ctx context.Context, _ Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInbox(t *testing.T) {
	c := &fakeClock{now: t0}
	in := NewInbox(2, c.Now)
	if in.Name() != "ui" {
		t.Fatal(in.Name())
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := in.Send(context.Background(), Event{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	items := in.List(false)
	if len(items) != 2 || items[0].Event.ID != "c" {
		t.Fatalf("newest first and bounded: %+v", items)
	}
	if in.UnreadCount() != 2 {
		t.Fatal("unread")
	}
	if !in.MarkRead("c") || in.UnreadCount() != 1 {
		t.Fatal("mark read")
	}
	if got := in.List(true); len(got) != 1 || got[0].Event.ID != "b" {
		t.Fatalf("unread only: %+v", got)
	}
	if !in.Ack("b") {
		t.Fatal("ack")
	}
	it := in.List(false)[1]
	if !it.Acked || !it.Read || !it.AckedAt.Equal(t0) {
		t.Fatalf("ack implies read: %+v", it)
	}
	if in.MarkRead("zzz") || in.Ack("zzz") {
		t.Fatal("unknown id")
	}
}
