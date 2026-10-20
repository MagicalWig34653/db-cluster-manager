// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event kinds produced by the notification subsystem itself. Events whose kind
// starts with KindPrefix are recorded but never routed, which prevents
// notification loops.
const (
	KindPrefix         = "notify."
	KindDeliveryOK     = "notify.delivery"
	KindDeliveryFailed = "notify.delivery_failed"
	KindSuppressed     = "notify.suppressed"
	KindEscalated      = "notify.escalated"
)

// Channel delivers an event to one destination.
type Channel interface {
	Name() string
	Send(ctx context.Context, e Event) error
}

// Dispatcher logs events, routes them and delivers them to channels. Every
// delivery outcome is itself recorded as an event.
type Dispatcher struct {
	// SendTimeout bounds each channel Send call (default 30s).
	SendTimeout time.Duration

	log      *Log
	router   *Router
	clock    Clock
	channels map[string]Channel
}

// NewDispatcher creates a Dispatcher with the given channels.
func NewDispatcher(log *Log, router *Router, clock Clock, channels ...Channel) *Dispatcher {
	if clock == nil {
		clock = time.Now
	}
	d := &Dispatcher{SendTimeout: 30 * time.Second, log: log, router: router, clock: clock, channels: map[string]Channel{}}
	for _, c := range channels {
		d.channels[c.Name()] = c
	}
	return d
}

// Handle appends e to the log first (so it is never lost), then routes and
// delivers it. It returns the stored event.
func (d *Dispatcher) Handle(ctx context.Context, e Event) Event {
	stored := d.log.Append(e)
	if strings.HasPrefix(stored.Kind, KindPrefix) {
		return stored
	}
	deliver, suppressed := d.router.Route(stored)
	for _, s := range suppressed {
		d.log.Append(Event{
			Severity: SeverityInfo, Kind: KindSuppressed, Resource: stored.Resource,
			Message: fmt.Sprintf("notification suppressed (%s) by rule %q", s.Reason, s.Rule),
			Details: map[string]string{"event_id": stored.ID, "rule": s.Rule, "reason": s.Reason},
		})
	}
	for _, dec := range deliver {
		out := stored.clone()
		if dec.Suppressed > 0 {
			if out.Details == nil {
				out.Details = map[string]string{}
			}
			out.Details["suppressed_count"] = strconv.Itoa(dec.Suppressed)
		}
		d.send(ctx, dec, out, stored.ID)
	}
	return stored
}

// Escalate delivers all escalations that are due. Call it periodically.
func (d *Dispatcher) Escalate(ctx context.Context) {
	for _, dec := range d.router.Due() {
		out := dec.Event.clone()
		if out.Details == nil {
			out.Details = map[string]string{}
		}
		out.Details["escalated"] = "true"
		d.log.Append(Event{
			Severity: SeverityWarning, Kind: KindEscalated, Resource: out.Resource,
			Message: fmt.Sprintf("notification not acknowledged, escalating to %q", dec.Channel),
			Details: map[string]string{"event_id": out.ID, "rule": dec.Rule, "channel": dec.Channel},
		})
		d.send(ctx, dec, out, out.ID)
	}
}

func (d *Dispatcher) send(ctx context.Context, dec Decision, e Event, origID string) {
	err := d.trySend(ctx, dec.Channel, e)
	res := Event{
		Resource: e.Resource,
		Details:  map[string]string{"event_id": origID, "channel": dec.Channel, "rule": dec.Rule},
	}
	if err != nil {
		res.Severity, res.Kind = SeverityWarning, KindDeliveryFailed
		res.Message = fmt.Sprintf("delivery to %q failed", dec.Channel)
		res.Details["error"] = err.Error()
	} else {
		res.Severity, res.Kind = SeverityInfo, KindDeliveryOK
		res.Message = fmt.Sprintf("delivered to %q", dec.Channel)
	}
	d.log.Append(res)
}

func (d *Dispatcher) trySend(ctx context.Context, name string, e Event) (err error) {
	ch, ok := d.channels[name]
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("channel panicked: %v", r)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, d.SendTimeout)
	defer cancel()
	return ch.Send(cctx, e)
}
