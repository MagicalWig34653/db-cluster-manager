// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// Querier is the read side of the metric store.
type Querier interface {
	Query(ctx context.Context, q metrics.Query) (*metrics.Result, error)
}

// Config configures an Evaluator.
type Config struct {
	Rules   []Rule
	Querier Querier
	// Authorizer is mandatory: evaluation sees only what it allows.
	Authorizer metrics.Authorizer
	// Step is the query step (default 1s: steps are aligned down to a multiple of the step, so a coarse step would hide the newest samples for up to one step). Window defaults to 2 minutes.
	Step          time.Duration
	DefaultWindow time.Duration
	Now           func() time.Time
	MaxInstances  int           // per evaluator, default 10000; excess is dropped and reported
	ForgetAfter   time.Duration // drop vanished instances after this (default 24h)
}

type instState struct {
	inst        Instance
	pendingFrom time.Time
	unknownFrom time.Time
	unkNotified bool
	lastNotify  time.Time
	seen        time.Time
	labels      metrics.Labels
}

// Evaluator holds rule state across evaluations. Not safe for concurrent
// Evaluate calls; Instances may be called concurrently.
type Evaluator struct {
	cfg   Config
	mu    sync.Mutex
	state map[string]*instState
}

// New validates cfg.
func New(cfg Config) (*Evaluator, error) {
	if cfg.Querier == nil || cfg.Authorizer == nil {
		return nil, errors.New("alerts: Querier and Authorizer are required")
	}
	seen := map[string]bool{}
	for _, r := range cfg.Rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("alerts: duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
	}
	if cfg.Step <= 0 {
		cfg.Step = time.Second
	}
	if cfg.DefaultWindow <= 0 {
		cfg.DefaultWindow = 2 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxInstances <= 0 {
		cfg.MaxInstances = 10000
	}
	if cfg.ForgetAfter <= 0 {
		cfg.ForgetAfter = 24 * time.Hour
	}
	e := &Evaluator{cfg: cfg, state: map[string]*instState{}}
	return e, nil
}

const noSeries = "(no series)"

// Evaluate runs all rules once and returns the events to dispatch. A query
// error turns every instance of that rule unknown (never ok) and is returned
// joined, together with the events of the other rules.
func (e *Evaluator) Evaluate(ctx context.Context) ([]notify.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.Now()
	var events []notify.Event
	var errs []error
	for _, r := range e.cfg.Rules {
		ev, err := e.evalRule(ctx, r, now)
		events = append(events, ev...)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.ID, err))
		}
	}
	for k, s := range e.state {
		if now.Sub(s.seen) > e.cfg.ForgetAfter && s.inst.State != StateFiring {
			delete(e.state, k)
		}
	}
	return events, errors.Join(errs...)
}

type obs struct {
	key     string
	labels  metrics.Labels
	v       float64
	known   bool // false: data cannot support a decision
	present bool
	qfail   bool // the query failed: nothing can be concluded
}

func (e *Evaluator) evalRule(ctx context.Context, r Rule, now time.Time) ([]notify.Event, error) {
	sel, _ := metrics.ParseSelector(r.Selector)
	fn := r.Func
	if fn == "" {
		fn = metrics.FuncLast
	}
	win := r.Window
	if win <= 0 {
		win = e.cfg.DefaultWindow
	}
	// Steps are aligned down, so evaluate at the first step boundary at or
	// after now: the window then covers every sample up to now.
	stepMs := e.cfg.Step.Milliseconds()
	end := (now.UnixMilli() + stepMs - 1) / stepMs * stepMs
	res, qerr := e.cfg.Querier.Query(ctx, metrics.Query{
		Selector: sel, Start: end, End: end, Step: e.cfg.Step.Milliseconds(), Window: win.Milliseconds(),
		Func: fn, Authorizer: e.cfg.Authorizer,
	})
	var observed []obs
	if qerr == nil {
		for _, s := range res.Series {
			o := obs{key: s.Labels.Key(), labels: s.Labels, present: true}
			if n := len(s.Points); n > 0 {
				p := s.Points[n-1]
				o.v, o.known = p.V, p.State == metrics.StateOK
			}
			observed = append(observed, o)
		}
	}
	got := map[string]bool{}
	for _, o := range observed {
		got[o.key] = true
	}
	// Previously seen instances of this rule that are now missing (or all of
	// them when the query failed) are unknown.
	for k, s := range e.state {
		if s.inst.Rule == r.ID && k != r.ID+"\x00"+noSeries && !got[s.inst.Series] {
			observed = append(observed, obs{key: s.inst.Series, labels: s.labels, qfail: qerr != nil})
		}
	}
	if len(observed) == 0 {
		observed = append(observed, obs{key: noSeries})
	} else {
		delete(e.state, r.ID+"\x00"+noSeries)
	}
	var events []notify.Event
	for _, o := range observed {
		sk := r.ID + "\x00" + o.key
		st := e.state[sk]
		if st == nil {
			if len(e.state) >= e.cfg.MaxInstances {
				continue
			}
			st = &instState{inst: Instance{Rule: r.ID, Series: o.key, State: StateUnknown, Since: now}, unknownFrom: now, labels: o.labels}
			if o.labels != nil {
				st.inst.Series = o.labels.Key()
				st.inst.Labels = map[string]string{}
				for _, l := range o.labels {
					st.inst.Labels[l.Name] = l.Value
				}
			}
			e.state[sk] = st
		}
		st.seen = now
		if ev := e.step(r, st, o, now); ev != nil {
			events = append(events, *ev)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Resource < events[j].Resource })
	return events, qerr
}

func (e *Evaluator) setState(st *instState, s State, now time.Time) {
	if st.inst.State != s {
		st.inst.State, st.inst.Since = s, now
	}
}

func (e *Evaluator) step(r Rule, st *instState, o obs, now time.Time) *notify.Event {
	cond, decided := false, o.known && !o.qfail
	switch {
	case o.qfail:
		decided = false
	case r.Absent:
		switch {
		case o.known:
			cond, decided = false, true
			st.inst.Value, st.inst.HasVal = o.v, true
		case o.labels != nil:
			// A series that existed has no usable data now.
			cond, decided = true, true
		}
	case decided:
		cond = r.Cmp.test(o.v, r.Threshold)
		st.inst.Value, st.inst.HasVal = o.v, true
	}

	if !decided {
		if st.inst.State != StateUnknown && st.inst.State != StateFiring {
			st.unknownFrom, st.unkNotified = now, false
		}
		if st.inst.State == StateFiring {
			return e.repeat(r, st, now) // stays firing
		}
		e.setState(st, StateUnknown, now)
		if st.unknownFrom.IsZero() {
			st.unknownFrom = now
		}
		if !st.unkNotified && now.Sub(st.unknownFrom) >= r.For {
			st.unkNotified = true
			return e.event(r, st, KindUnknown, notify.SeverityUnknown, "no usable data; state unknown", now)
		}
		return nil
	}
	st.unknownFrom, st.unkNotified = time.Time{}, false
	if !cond {
		was := st.inst.State
		st.pendingFrom = time.Time{}
		e.setState(st, StateOK, now)
		if was == StateFiring {
			st.lastNotify = time.Time{}
			return e.event(r, st, KindResolved, notify.SeverityInfo, "condition cleared", now)
		}
		return nil
	}
	switch st.inst.State {
	case StateFiring:
		return e.repeat(r, st, now)
	case StatePending:
	default:
		st.pendingFrom = now
		e.setState(st, StatePending, now)
	}
	if now.Sub(st.pendingFrom) >= r.For {
		e.setState(st, StateFiring, now)
		st.lastNotify = now
		return e.event(r, st, KindFiring, r.Severity, "condition met", now)
	}
	return nil
}

func (e *Evaluator) repeat(r Rule, st *instState, now time.Time) *notify.Event {
	if r.Cooldown > 0 && now.Sub(st.lastNotify) >= r.Cooldown {
		st.lastNotify = now
		return e.event(r, st, KindFiring, r.Severity, "still firing", now)
	}
	return nil
}

func (e *Evaluator) event(r Rule, st *instState, kind string, sev notify.Severity, what string, now time.Time) *notify.Event {
	d := map[string]string{"rule": r.ID, "state": string(st.inst.State), "series": st.inst.Series}
	for k, v := range st.inst.Labels {
		switch k {
		case "swarm", "cluster", "node", "host", "proxy", "container":
			d[k] = v
		}
	}
	if st.inst.HasVal {
		d["value"] = strconv.FormatFloat(st.inst.Value, 'g', 6, 64)
	}
	res := ""
	if s := d["swarm"]; s != "" {
		res = "swarm/" + s
		if c := d["cluster"]; c != "" {
			res += "/cluster/" + c
			if n := d["node"]; n != "" {
				res += "/node/" + n
			}
		}
	}
	msg := fmt.Sprintf("%s: %s", r.Summary, what)
	if st.inst.HasVal && kind != KindUnknown {
		msg += " (value " + d["value"] + ")"
	}
	return &notify.Event{Time: now, Severity: sev, Kind: kind, Resource: res, Message: msg, Details: d}
}

// Instances returns the current instance states sorted by rule and series.
func (e *Evaluator) Instances() []Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Instance, 0, len(e.state))
	for _, s := range e.state {
		out = append(out, s.inst)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Series < out[j].Series
	})
	return out
}
