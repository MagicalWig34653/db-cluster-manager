// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Mode is the patch mode of a host or group.
type Mode string

// Modes. The zero value (and anything unset) means report only.
const (
	ModeReportOnly       Mode = "report_only"
	ModeApprovalRequired Mode = "approval_required"
)

// RebootPolicy says what may happen after patching. The default is never.
type RebootPolicy string

// Reboot policies.
const (
	// RebootNever: the manager only reports reboot-required.
	RebootNever RebootPolicy = "never"
	// RebootWithPlan: a plan may include a reboot step; the approval of the
	// plan is the approval of the reboot. Runs only inside a window.
	RebootWithPlan RebootPolicy = "with_plan"
	// RebootAuto: the policy itself allows a reboot after an approved
	// apply when the host reports reboot-required, inside a window.
	RebootAuto RebootPolicy = "auto_in_window"
)

// Errors of policy evaluation.
var (
	ErrReportOnly    = errors.New("patch: policy is report-only; applying is not allowed")
	ErrOutsideWindow = errors.New("patch: outside the maintenance window")
	ErrRebootDenied  = errors.New("patch: policy does not allow a reboot")
)

// Window is a recurring maintenance window in a time zone.
type Window struct {
	Days            []string `json:"days"`            // mon..sun; empty = every day
	Start           string   `json:"start"`           // HH:MM
	DurationMinutes int      `json:"durationMinutes"` // 1..1440
	Zone            string   `json:"zone,omitempty"`  // IANA name; default UTC
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func dayIndex(s string) int {
	for i, d := range dayNames {
		if strings.EqualFold(d, s) {
			return i
		}
	}
	return -1
}

func (w Window) parse() (loc *time.Location, startMin int, days map[int]bool, err error) {
	loc = time.UTC
	if w.Zone != "" {
		if loc, err = time.LoadLocation(w.Zone); err != nil {
			return nil, 0, nil, fmt.Errorf("%w: zone %q", ErrInvalid, w.Zone)
		}
	}
	h, m, ok := strings.Cut(w.Start, ":")
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if !ok || e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(w.Start) != 5 {
		return nil, 0, nil, fmt.Errorf("%w: window start %q", ErrInvalid, w.Start)
	}
	if w.DurationMinutes < 1 || w.DurationMinutes > 1440 {
		return nil, 0, nil, fmt.Errorf("%w: window duration", ErrInvalid)
	}
	days = map[int]bool{}
	for _, d := range w.Days {
		i := dayIndex(d)
		if i < 0 {
			return nil, 0, nil, fmt.Errorf("%w: day %q", ErrInvalid, d)
		}
		days[i] = true
	}
	if len(days) == 0 {
		for i := 0; i < 7; i++ {
			days[i] = true
		}
	}
	return loc, hh*60 + mm, days, nil
}

// Validate checks the window.
func (w Window) Validate() error { _, _, _, err := w.parse(); return err }

// Contains reports whether t lies in an occurrence of the window (also used
// by the image update policies).
func (w Window) Contains(t time.Time) bool { return w.contains(t) }

// contains reports whether t lies in an occurrence of the window. The window
// that began on the previous calendar day is considered too (overnight).
func (w Window) contains(t time.Time) bool {
	loc, start, days, err := w.parse()
	if err != nil {
		return false
	}
	lt := t.In(loc)
	for back := 0; back <= 1; back++ {
		d := lt.AddDate(0, 0, -back)
		if !days[int(d.Weekday())] {
			continue
		}
		begin := time.Date(d.Year(), d.Month(), d.Day(), start/60, start%60, 0, 0, loc)
		if !lt.Before(begin) && lt.Before(begin.Add(time.Duration(w.DurationMinutes)*time.Minute)) {
			return true
		}
	}
	return false
}

// Policy governs one host or group. Windows empty means no time restriction;
// the approval is then the only gate.
type Policy struct {
	Mode         Mode         `json:"mode"`
	SecurityOnly bool         `json:"securityOnly,omitempty"`
	Windows      []Window     `json:"windows,omitempty"`
	Reboot       RebootPolicy `json:"reboot,omitempty"`
	// ContainerRuntime is the mode for the container-runtime package class
	// (Docker engine, containerd, plugins). Anything unset is report_only:
	// engine updates are never applied unless this is explicitly
	// approval_required, and then only by an approved plan behind the
	// engine guard.
	ContainerRuntime Mode `json:"containerRuntime,omitempty"`
	// ReportEveryMinutes is the pull schedule; 0 means the default (360).
	ReportEveryMinutes int `json:"reportEveryMinutes,omitempty"`
}

// DefaultReportEvery is the default pull interval.
const DefaultReportEvery = 360 * time.Minute

// Normalized returns the policy with defaults filled in.
func (p Policy) Normalized() Policy {
	if p.Mode == "" {
		p.Mode = ModeReportOnly
	}
	if p.Reboot == "" {
		p.Reboot = RebootNever
	}
	if p.ContainerRuntime == "" {
		p.ContainerRuntime = ModeReportOnly
	}
	return p
}

// Validate checks the policy.
func (p Policy) Validate() error {
	p = p.Normalized()
	if p.Mode != ModeReportOnly && p.Mode != ModeApprovalRequired {
		return fmt.Errorf("%w: mode %q", ErrInvalid, p.Mode)
	}
	if p.ContainerRuntime != ModeReportOnly && p.ContainerRuntime != ModeApprovalRequired {
		return fmt.Errorf("%w: container-runtime mode %q", ErrInvalid, p.ContainerRuntime)
	}
	if p.Reboot != RebootNever && p.Reboot != RebootWithPlan && p.Reboot != RebootAuto {
		return fmt.Errorf("%w: reboot policy %q", ErrInvalid, p.Reboot)
	}
	if p.ReportEveryMinutes < 0 || (p.ReportEveryMinutes > 0 && p.ReportEveryMinutes < 5) || p.ReportEveryMinutes > 7*24*60 {
		return fmt.Errorf("%w: report interval", ErrInvalid)
	}
	if len(p.Windows) > 16 {
		return fmt.Errorf("%w: too many windows", ErrInvalid)
	}
	for _, w := range p.Windows {
		if err := w.Validate(); err != nil {
			return err
		}
	}
	if p.Reboot != RebootNever && len(p.Windows) == 0 {
		return fmt.Errorf("%w: a reboot policy other than never needs a maintenance window", ErrInvalid)
	}
	return nil
}

// ReportEvery returns the effective pull interval.
func (p Policy) ReportEvery() time.Duration {
	if p.ReportEveryMinutes > 0 {
		return time.Duration(p.ReportEveryMinutes) * time.Minute
	}
	return DefaultReportEvery
}

// InWindow reports whether now is inside a window (true when none is set).
func (p Policy) InWindow(now time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	for _, w := range p.Windows {
		if w.contains(now) {
			return true
		}
	}
	return false
}

// NextWindow returns the next window start strictly after now, scanning 8
// days in one-minute steps' worth of day granularity.
func (p Policy) NextWindow(now time.Time) (time.Time, bool) {
	var best time.Time
	for _, w := range p.Windows {
		loc, start, days, err := w.parse()
		if err != nil {
			continue
		}
		lt := now.In(loc)
		for d := 0; d <= 8; d++ {
			day := lt.AddDate(0, 0, d)
			if !days[int(day.Weekday())] {
				continue
			}
			b := time.Date(day.Year(), day.Month(), day.Day(), start/60, start%60, 0, 0, loc)
			if b.After(now) && (best.IsZero() || b.Before(best)) {
				best = b
			}
		}
	}
	return best, !best.IsZero()
}

// CheckApply returns nil when an approved plan may start now.
func (p Policy) CheckApply(now time.Time) error {
	p = p.Normalized()
	if p.Mode == ModeReportOnly {
		return ErrReportOnly
	}
	if !p.InWindow(now) {
		return ErrOutsideWindow
	}
	return nil
}

// CheckReboot returns nil when the plan's reboot may run now. planReboot is
// the plan's approved reboot step; required is the host's reboot-required.
func (p Policy) CheckReboot(now time.Time, planReboot bool, required Tristate) error {
	p = p.Normalized()
	if p.Mode == ModeReportOnly {
		return ErrReportOnly
	}
	switch p.Reboot {
	case RebootWithPlan:
		if !planReboot {
			return ErrRebootDenied
		}
	case RebootAuto:
		if !planReboot && required != Yes {
			return ErrRebootDenied
		}
	default:
		return ErrRebootDenied
	}
	if !p.InWindow(now) {
		return ErrOutsideWindow
	}
	return nil
}

// Resolve picks the effective policy: host override, then group, then the
// default. The most specific policy applies as a whole; there is no merge,
// so a stricter group policy cannot be loosened field by field by accident.
func Resolve(host, group *Policy, def Policy) Policy {
	switch {
	case host != nil:
		return host.Normalized()
	case group != nil:
		return group.Normalized()
	}
	return def.Normalized()
}
