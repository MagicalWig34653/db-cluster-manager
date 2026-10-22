// SPDX-License-Identifier: AGPL-3.0-only

// Package alerts evaluates threshold and absence rules over the metric store
// and converts state changes into notify.Events.
//
// Semantics (documented behavior):
//
//   - A rule selects series with a selector, reduces the last Window of each
//     series with Func and compares the result to Threshold. Each matching
//     series is an independent instance.
//   - Instance states: ok, pending (condition true, For not yet elapsed),
//     firing, unknown. Unknown means the data cannot support a decision: the
//     query step is no_data or unavailable, or a previously seen series has
//     vanished. Unknown is never reported as ok. A firing instance that turns
//     unknown stays firing (it is not resolved on missing data).
//   - An instance that stays unknown for For emits one alert.unknown event
//     with severity unknown (which notification routing treats as at least a
//     warning).
//   - Absence rules (Absent) fire when a series that should exist has no data;
//     a series that never existed cannot be detected and is reported by the
//     rule-level instance "(no series)" as unknown.
//   - Re-notification of a still firing instance happens at most once per
//     Cooldown (0 disables repeats).
//   - Reads go through the metric store's Query with the Authorizer given to
//     the Evaluator, so evaluation never sees series its identity may not read.
package alerts

import (
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// Comparator compares a value with the threshold.
type Comparator string

// Comparators.
const (
	Above        Comparator = ">"
	AboveOrEqual Comparator = ">="
	Below        Comparator = "<"
	BelowOrEqual Comparator = "<="
	Equal        Comparator = "=="
	NotEqual     Comparator = "!="
)

func (c Comparator) test(v, th float64) bool {
	switch c {
	case Above:
		return v > th
	case AboveOrEqual:
		return v >= th
	case Below:
		return v < th
	case BelowOrEqual:
		return v <= th
	case Equal:
		return v == th
	case NotEqual:
		return v != th
	}
	return false
}

// Rule is one alert rule.
type Rule struct {
	ID       string
	Summary  string // human text; the event message adds the instance and value
	Selector string
	Func     metrics.Func // default last
	Window   time.Duration
	// Cmp and Threshold define the firing condition. Ignored when Absent.
	Cmp       Comparator
	Threshold float64
	// Absent makes the rule fire when the series has no data.
	Absent   bool
	For      time.Duration
	Severity notify.Severity
	Cooldown time.Duration
}

// Validate checks the rule.
func (r Rule) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("alerts: rule %q: %s", r.ID, fmt.Sprintf(f, a...)) }
	if r.ID == "" || r.Summary == "" {
		return errors.New("alerts: rule needs id and summary")
	}
	if _, err := metrics.ParseSelector(r.Selector); err != nil {
		return bad("%v", err)
	}
	if r.Func != "" {
		f, err := metrics.ParseFunc(string(r.Func))
		if err != nil || f == metrics.FuncQuantile {
			return bad("unsupported function %q", r.Func)
		}
	}
	if r.Window < 0 || r.For < 0 || r.Cooldown < 0 {
		return bad("negative duration")
	}
	if !r.Absent {
		switch r.Cmp {
		case Above, AboveOrEqual, Below, BelowOrEqual, Equal, NotEqual:
		default:
			return bad("unknown comparator %q", r.Cmp)
		}
	}
	if r.Severity != notify.SeverityInfo && r.Severity != notify.SeverityWarning &&
		r.Severity != notify.SeverityError && r.Severity != notify.SeverityCritical {
		return bad("severity must be info, warning, error or critical")
	}
	return nil
}

// State of one instance.
type State string

// States.
const (
	StateOK      State = "ok"
	StatePending State = "pending"
	StateFiring  State = "firing"
	StateUnknown State = "unknown"
)

// Event kinds produced by the evaluator.
const (
	KindFiring   = "alert.firing"
	KindResolved = "alert.resolved"
	KindUnknown  = "alert.unknown"
)

// Instance is the externally visible state of one rule/series pair.
type Instance struct {
	Rule   string
	Series string // label set rendering, or "(no series)"
	Labels map[string]string
	State  State
	Since  time.Time
	Value  float64 // meaningful for ok, pending and firing
	HasVal bool
}
