// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrSchedule reports an invalid schedule spec.
var ErrSchedule = errors.New("dbbackup: invalid schedule")

// ErrNoNextRun means the spec never matches within the search horizon
// (for example "0 0 30 2 *").
var ErrNoNextRun = errors.New("dbbackup: schedule has no next run")

// Schedule is a parsed five-field cron spec: minute hour day-of-month month
// day-of-week. Supported: "*", numbers, ranges "a-b", steps "*/n", "a-b/n",
// "a/n", lists "a,b", month and weekday names (jan..dec, sun..sat; 7 is
// also Sunday), and the macros @yearly, @monthly, @weekly, @daily, @hourly.
// Day-of-month and day-of-week follow cron semantics: if both are
// restricted, a day matches when either matches.
type Schedule struct {
	spec   string
	minute bitset
	hour   bitset
	dom    bitset
	month  bitset
	dow    bitset
	domAll bool
	dowAll bool
	loc    *time.Location
}

type bitset uint64

func (b bitset) has(i int) bool { return b&(1<<uint(i)) != 0 }

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// ParseSchedule parses spec in loc (nil means UTC).
func ParseSchedule(spec string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	s := Schedule{spec: strings.TrimSpace(spec), loc: loc}
	body := s.spec
	if m, ok := macros[strings.ToLower(body)]; ok {
		body = m
	}
	f := strings.Fields(body)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("%w: %q needs 5 fields, has %d", ErrSchedule, spec, len(f))
	}
	var err error
	if s.minute, _, err = parseField(f[0], 0, 59, nil); err != nil {
		return Schedule{}, fmt.Errorf("%w: minute: %v", ErrSchedule, err)
	}
	if s.hour, _, err = parseField(f[1], 0, 23, nil); err != nil {
		return Schedule{}, fmt.Errorf("%w: hour: %v", ErrSchedule, err)
	}
	if s.dom, s.domAll, err = parseField(f[2], 1, 31, nil); err != nil {
		return Schedule{}, fmt.Errorf("%w: day of month: %v", ErrSchedule, err)
	}
	if s.month, _, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return Schedule{}, fmt.Errorf("%w: month: %v", ErrSchedule, err)
	}
	if s.dow, s.dowAll, err = parseField(f[4], 0, 7, dowNames); err != nil {
		return Schedule{}, fmt.Errorf("%w: day of week: %v", ErrSchedule, err)
	}
	if s.dow.has(7) {
		s.dow = s.dow&^(1<<7) | 1
	}
	return s, nil
}

// MustParseSchedule panics on error (tests and constants).
func MustParseSchedule(spec string) Schedule {
	s, err := ParseSchedule(spec, nil)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the spec as given.
func (s Schedule) String() string { return s.spec }

func parseField(f string, lo, hi int, names map[string]int) (bitset, bool, error) {
	var b bitset
	all := f == "*"
	for part := range strings.SplitSeq(f, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 || n > hi-lo+1 {
				return 0, false, fmt.Errorf("step %q", stepS)
			}
			step = n
		}
		var a, z int
		switch {
		case rng == "*":
			a, z = lo, hi
			if hi == 7 {
				z = 6 // "*" for weekdays is 0-6
			}
		case strings.Contains(rng, "-"):
			as, zs, _ := strings.Cut(rng, "-")
			var err error
			if a, err = value(as, lo, hi, names); err != nil {
				return 0, false, err
			}
			if z, err = value(zs, lo, hi, names); err != nil {
				return 0, false, err
			}
			if z < a {
				return 0, false, fmt.Errorf("range %q is reversed", rng)
			}
		default:
			v, err := value(rng, lo, hi, names)
			if err != nil {
				return 0, false, err
			}
			a, z = v, v
			if hasStep {
				z = hi
			}
		}
		for i := a; i <= z; i += step {
			b |= 1 << uint(i)
		}
	}
	return b, all, nil
}

func value(s string, lo, hi int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("value %q not in %d-%d", s, lo, hi)
	}
	return v, nil
}

func (s Schedule) dayMatches(t time.Time) bool {
	d, w := s.dom.has(t.Day()), s.dow.has(int(t.Weekday()))
	switch {
	case s.domAll && s.dowAll:
		return true
	case s.domAll:
		return w
	case s.dowAll:
		return d
	}
	return d || w
}

// Next returns the first run time strictly after t. Times are minute
// aligned in the schedule's location. A wall time skipped by a DST change
// does not run; a wall time repeated by one runs at least once (twice when
// the minute and hour both match in both occurrences).
func (s Schedule) Next(t time.Time) (time.Time, error) {
	t = t.In(s.loc)
	// Start at the next whole minute.
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, s.loc).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case !s.month.has(int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, s.loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.loc)
		case !s.hour.has(t.Hour()):
			n := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, s.loc)
			if !n.After(t) { // DST fold: the next wall hour maps back
				n = t.Add(time.Hour).Truncate(time.Minute)
			}
			t = n
		case !s.minute.has(t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: %q within 5 years after %s", ErrNoNextRun, s.spec, t.Format(time.RFC3339))
}

// MissedPolicy decides what happens to runs missed while the manager was
// down or the previous run was still active.
type MissedPolicy string

// Missed-run policies.
const (
	// MissedSkip drops missed runs and waits for the next scheduled time.
	MissedSkip MissedPolicy = "skip"
	// MissedRunOnce runs once as soon as possible for any number of missed
	// runs (never a burst of catch-up runs).
	MissedRunOnce MissedPolicy = "run-once"
)

// Decision is the outcome of evaluating a schedule.
type Decision struct {
	// Run: start a run now. ScheduledFor is the newest due scheduled time
	// (zero if none is due); record it as the new "last" time whether or
	// not a run starts.
	Run          bool      `json:"run"`
	ScheduledFor time.Time `json:"scheduled_for,omitzero"`
	// Missed counts scheduled times older than the grace period that were
	// not run (capped at 1000).
	Missed int       `json:"missed"`
	Next   time.Time `json:"next"`
}

// Policy for one schedule (B01: global defaults, per cluster/database
// overrides resolve to one of these).
type SchedulePolicy struct {
	Schedule Schedule
	Missed   MissedPolicy
	// Grace is how late a run may start and still count as on time.
	// Default 10 minutes.
	Grace time.Duration
}

// Evaluate decides at now, given last (the scheduled time of the previous
// run, or the schedule's creation time). It is pure; the caller injects the
// clock.
func (p SchedulePolicy) Evaluate(last, now time.Time) (Decision, error) {
	grace := p.Grace
	if grace <= 0 {
		grace = 10 * time.Minute
	}
	switch p.Missed {
	case MissedSkip, MissedRunOnce:
	default:
		return Decision{}, fmt.Errorf("%w: missed-run policy %q", ErrSchedule, p.Missed)
	}
	first, err := p.Schedule.Next(last)
	if err != nil {
		return Decision{}, err
	}
	if first.After(now) {
		return Decision{Next: first}, nil
	}
	// Walk the scheduled times up to now: latest is the newest one due.
	latest, n := first, 0
	for {
		nx, err := p.Schedule.Next(latest)
		if err != nil || nx.After(now) || n >= 1000 {
			break
		}
		latest, n = nx, n+1
	}
	next, err := p.Schedule.Next(now)
	if err != nil {
		return Decision{}, err
	}
	onTime := now.Sub(latest) <= grace
	// ScheduledFor is set whenever a time was due, also when it is skipped,
	// so the caller can advance past it.
	d := Decision{Missed: n, Next: next, ScheduledFor: latest}
	if !onTime {
		d.Missed++
	}
	d.Run = onTime || p.Missed == MissedRunOnce
	return d, nil
}

// Clock is the injected time source of a Scheduler.
type Clock interface{ Now() time.Time }

// Scheduler evaluates a policy against a clock and remembers the last run.
type Scheduler struct {
	Policy SchedulePolicy
	Clock  Clock
	Last   time.Time
}

// Tick evaluates now and advances Last past every due time (run or skipped).
func (s *Scheduler) Tick() (Decision, error) {
	d, err := s.Policy.Evaluate(s.Last, s.Clock.Now())
	if err != nil {
		return d, err
	}
	if !d.ScheduledFor.IsZero() {
		s.Last = d.ScheduledFor
	}
	return d, nil
}
