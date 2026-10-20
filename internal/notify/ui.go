// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"context"
	"sync"
	"time"
)

// InboxItem is one UI notification with its read/ack state.
type InboxItem struct {
	Event   Event
	Read    bool
	Acked   bool
	AckedAt time.Time
}

// Inbox is the in-memory UI notification channel (name "ui"). It is bounded;
// the oldest items are dropped first.
type Inbox struct {
	mu    sync.Mutex
	cap   int
	clock Clock
	items []InboxItem // oldest first
}

// NewInbox creates an Inbox holding at most capacity items.
func NewInbox(capacity int, clock Clock) *Inbox {
	if capacity < 1 {
		capacity = 1
	}
	if clock == nil {
		clock = time.Now
	}
	return &Inbox{cap: capacity, clock: clock}
}

// Name implements Channel.
func (i *Inbox) Name() string { return "ui" }

// Send implements Channel.
func (i *Inbox) Send(_ context.Context, e Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.items = append(i.items, InboxItem{Event: e.clone()})
	if over := len(i.items) - i.cap; over > 0 {
		i.items = append([]InboxItem(nil), i.items[over:]...)
	}
	return nil
}

// List returns items newest first, optionally only unread ones.
func (i *Inbox) List(unreadOnly bool) []InboxItem {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []InboxItem
	for n := len(i.items) - 1; n >= 0; n-- {
		it := i.items[n]
		if unreadOnly && it.Read {
			continue
		}
		it.Event = it.Event.clone()
		out = append(out, it)
	}
	return out
}

// UnreadCount returns the number of unread items.
func (i *Inbox) UnreadCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := 0
	for _, it := range i.items {
		if !it.Read {
			n++
		}
	}
	return n
}

func (i *Inbox) find(id string) *InboxItem {
	for n := len(i.items) - 1; n >= 0; n-- {
		if i.items[n].Event.ID == id {
			return &i.items[n]
		}
	}
	return nil
}

// MarkRead marks the item with the given event ID as read.
func (i *Inbox) MarkRead(id string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	it := i.find(id)
	if it == nil {
		return false
	}
	it.Read = true
	return true
}

// Ack acknowledges an item (which also marks it read). Callers should also
// call Router.Ack with the notification key to cancel escalation.
func (i *Inbox) Ack(id string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	it := i.find(id)
	if it == nil {
		return false
	}
	if !it.Acked {
		it.Acked, it.AckedAt = true, i.clock()
	}
	it.Read = true
	return true
}
