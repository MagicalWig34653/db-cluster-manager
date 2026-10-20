// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Suppression reasons.
const (
	ReasonCooldown = "cooldown"
	ReasonFlapping = "flapping"
)

// Matcher selects events. Empty fields match everything. Kinds entries match
// exactly, or as a prefix when they end in "*".
type Matcher struct {
	Kinds          []string
	ResourcePrefix string
	MinSeverity    Severity
}

// Matches reports whether e is selected.
func (m Matcher) Matches(e Event) bool {
	if len(m.Kinds) > 0 {
		ok := false
		for _, k := range m.Kinds {
			if strings.HasSuffix(k, "*") {
				ok = strings.HasPrefix(e.Kind, strings.TrimSuffix(k, "*"))
			} else {
				ok = e.Kind == k
			}
			if ok {
				break
			}
		}
		if !ok {
			return false
		}
	}
	if m.ResourcePrefix != "" && !strings.HasPrefix(e.Resource, m.ResourcePrefix) {
		return false
	}
	if m.MinSeverity != SeverityUnknown && e.Severity.rank() < m.MinSeverity.rank() {
		return false
	}
	return true
}

// Rule routes matching events to channels.
//
// GroupBy lists the fields forming the deduplication group: "kind",
// "resource", "severity" or "detail:<key>". An empty list puts all events
// matching the rule into one group. Cooldown suppresses further deliveries of
// the same group for that long after a delivery. If FlapMax > 0, a group that
// receives more than FlapMax events within FlapWindow is considered flapping
// and suppressed until it has been quiet for FlapWindow. If EscalateTo is set,
// a delivery not acknowledged within EscalateAfter is escalated to that channel.
type Rule struct {
	Name          string
	Match         Matcher
	Channels      []string
	Cooldown      time.Duration
	GroupBy       []string
	FlapWindow    time.Duration
	FlapMax       int
	EscalateTo    string
	EscalateAfter time.Duration
}

// Decision is a routing outcome: deliver to Channel.
type Decision struct {
	Rule       string
	Key        string
	Channel    string
	Suppressed int   // events suppressed since the previous delivery of this group
	Escalated  bool  // true for escalation deliveries
	Event      Event // the original event, set for escalations
}

// Suppression records why a matching rule did not deliver.
type Suppression struct {
	Rule   string
	Key    string
	Reason string
}

type groupState struct {
	lastDelivery time.Time
	delivered    bool
	suppressed   int
	arrivals     []time.Time
}

type pending struct {
	rule      Rule
	event     Event
	since     time.Time
	escalated bool
}

// Router applies rules with an injected clock. It is pure in-memory logic and
// safe for concurrent use.
type Router struct {
	mu      sync.Mutex
	rules   []Rule
	clock   Clock
	groups  map[string]*groupState
	pending map[string]*pending
}

// NewRouter creates a Router.
func NewRouter(rules []Rule, clock Clock) *Router {
	if clock == nil {
		clock = time.Now
	}
	return &Router{
		rules:   append([]Rule(nil), rules...),
		clock:   clock,
		groups:  map[string]*groupState{},
		pending: map[string]*pending{},
	}
}

func groupKey(r Rule, e Event) string {
	parts := []string{r.Name}
	for _, f := range r.GroupBy {
		switch {
		case f == "kind":
			parts = append(parts, e.Kind)
		case f == "resource":
			parts = append(parts, e.Resource)
		case f == "severity":
			parts = append(parts, e.Severity.String())
		case strings.HasPrefix(f, "detail:"):
			parts = append(parts, e.Details[strings.TrimPrefix(f, "detail:")])
		}
	}
	return strings.Join(parts, "|")
}

// Route evaluates all rules for e.
func (r *Router) Route(e Event) (deliver []Decision, suppressed []Suppression) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	for _, rule := range r.rules {
		if !rule.Match.Matches(e) {
			continue
		}
		key := groupKey(rule, e)
		g := r.groups[key]
		if g == nil {
			g = &groupState{}
			r.groups[key] = g
		}
		if rule.FlapMax > 0 && rule.FlapWindow > 0 {
			kept := g.arrivals[:0]
			for _, t := range g.arrivals {
				if now.Sub(t) < rule.FlapWindow {
					kept = append(kept, t)
				}
			}
			g.arrivals = append(kept, now)
			if len(g.arrivals) > rule.FlapMax {
				g.suppressed++
				suppressed = append(suppressed, Suppression{rule.Name, key, ReasonFlapping})
				continue
			}
		}
		if rule.Cooldown > 0 && g.delivered && now.Sub(g.lastDelivery) < rule.Cooldown {
			g.suppressed++
			suppressed = append(suppressed, Suppression{rule.Name, key, ReasonCooldown})
			continue
		}
		g.delivered, g.lastDelivery = true, now
		count := g.suppressed
		g.suppressed = 0
		for _, ch := range rule.Channels {
			deliver = append(deliver, Decision{Rule: rule.Name, Key: key, Channel: ch, Suppressed: count})
		}
		if rule.EscalateTo != "" && rule.EscalateAfter > 0 {
			if p := r.pending[key]; p == nil || p.escalated {
				r.pending[key] = &pending{rule: rule, event: e.clone(), since: now}
			}
		}
	}
	return deliver, suppressed
}

// Ack acknowledges the notification group identified by key, cancelling a
// pending escalation. It reports whether anything was pending.
func (r *Router) Ack(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[key]
	delete(r.pending, key)
	return ok
}

// Due returns escalations whose acknowledgement deadline has passed. Each
// pending notification escalates at most once.
func (r *Router) Due() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	var out []Decision
	for key, p := range r.pending {
		if p.escalated || now.Sub(p.since) < p.rule.EscalateAfter {
			continue
		}
		p.escalated = true
		out = append(out, Decision{Rule: p.rule.Name, Key: key, Channel: p.rule.EscalateTo, Escalated: true, Event: p.event.clone()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
