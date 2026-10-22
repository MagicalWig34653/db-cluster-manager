// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"errors"
	"testing"
	"time"
)

func mustNext(t *testing.T, spec string, from time.Time) time.Time {
	t.Helper()
	s, err := ParseSchedule(spec, nil)
	if err != nil {
		t.Fatalf("%s: %v", spec, err)
	}
	n, err := s.Next(from)
	if err != nil {
		t.Fatalf("%s: %v", spec, err)
	}
	return n
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleNext(t *testing.T) {
	// 2026-10-22 is a Thursday.
	cases := []struct{ spec, from, want string }{
		{"30 2 * * *", "2026-10-22 01:00", "2026-10-22 02:30"},
		{"30 2 * * *", "2026-10-22 02:30", "2026-10-23 02:30"}, // strictly after
		{"*/15 * * * *", "2026-10-22 01:07", "2026-10-22 01:15"},
		{"0 */6 * * *", "2026-10-22 07:00", "2026-10-22 12:00"},
		{"0 3 * * mon-fri", "2026-10-23 04:00", "2026-10-26 03:00"}, // Fri after 3 -> Mon
		{"0 3 * * 7", "2026-10-22 00:00", "2026-10-25 03:00"},       // 7 = Sunday
		{"0 0 1 */3 *", "2026-10-22 00:00", "2027-01-01 00:00"},
		{"0 0 31 * *", "2026-11-01 00:00", "2026-12-31 00:00"}, // skips November
		{"0 0 29 feb *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 13 * fri", "2026-10-22 00:00", "2026-10-23 12:00"}, // dom OR dow: Friday 23rd
		{"5,10-12/2 1 * * *", "2026-10-22 01:06", "2026-10-22 01:10"},
		{"7/20 * * * *", "2026-10-22 01:28", "2026-10-22 01:47"},
		{"@daily", "2026-10-22 01:00", "2026-10-23 00:00"},
		{"@hourly", "2026-10-22 01:00", "2026-10-22 02:00"},
		{"@weekly", "2026-10-22 01:00", "2026-10-25 00:00"},
	}
	for _, c := range cases {
		if got := mustNext(t, c.spec, at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s from %s: %s, want %s", c.spec, c.from, got.Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
}

func TestScheduleParseErrors(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * foo *", "@sometimes"} {
		if _, err := ParseSchedule(bad, nil); !errors.Is(err, ErrSchedule) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	s := MustParseSchedule("0 0 30 2 *")
	if _, err := s.Next(t0); !errors.Is(err, ErrNoNextRun) {
		t.Fatalf("impossible date: %v", err)
	}
}

func TestScheduleTimezoneAndDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	s, _ := ParseSchedule("30 2 * * *", loc)
	// 2026-03-29: 02:00 -> 03:00 CEST, 02:30 does not exist that day.
	n, err := s.Next(time.Date(2026, 3, 29, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if n.Day() != 30 || n.Hour() != 2 || n.Minute() != 30 {
		t.Fatalf("spring forward: %s", n)
	}
	// 2026-10-25: 03:00 -> 02:00 CET; 02:30 occurs; it runs at least once.
	n, _ = s.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, loc))
	if n.Day() != 25 || n.Hour() != 2 || n.Minute() != 30 {
		t.Fatalf("fall back: %s", n)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestScheduleMissedRunPolicies(t *testing.T) {
	daily := MustParseSchedule("0 2 * * *")
	last := at("2026-10-20 02:00")

	// On time (within grace).
	d, err := SchedulePolicy{Schedule: daily, Missed: MissedSkip}.Evaluate(last, at("2026-10-21 02:05"))
	if err != nil || !d.Run || !d.ScheduledFor.Equal(at("2026-10-21 02:00")) || d.Missed != 0 {
		t.Fatalf("on time: %+v %v", d, err)
	}
	// Not yet due.
	d, _ = SchedulePolicy{Schedule: daily, Missed: MissedSkip}.Evaluate(last, at("2026-10-21 01:00"))
	if d.Run || !d.Next.Equal(at("2026-10-21 02:00")) {
		t.Fatalf("not due: %+v", d)
	}
	// Manager was down for three scheduled times (21st, 22nd, 23rd), now 23rd 09:00.
	now := at("2026-10-23 09:00")
	d, _ = SchedulePolicy{Schedule: daily, Missed: MissedSkip}.Evaluate(last, now)
	if d.Run || d.Missed != 3 || !d.Next.Equal(at("2026-10-24 02:00")) || !d.ScheduledFor.Equal(at("2026-10-23 02:00")) {
		t.Fatalf("skip: %+v", d)
	}
	d, _ = SchedulePolicy{Schedule: daily, Missed: MissedRunOnce}.Evaluate(last, now)
	if !d.Run || d.Missed != 3 || !d.ScheduledFor.Equal(at("2026-10-23 02:00")) {
		t.Fatalf("run-once: %+v", d)
	}
	// A late start inside the grace counts the earlier missed ones.
	d, _ = SchedulePolicy{Schedule: daily, Missed: MissedSkip, Grace: 8 * time.Hour}.Evaluate(last, now)
	if !d.Run || d.Missed != 2 {
		t.Fatalf("grace: %+v", d)
	}
	if _, err := (SchedulePolicy{Schedule: daily, Missed: "burst"}).Evaluate(last, now); !errors.Is(err, ErrSchedule) {
		t.Fatal(err)
	}
}

func TestSchedulerTickRunsOnceAfterOutage(t *testing.T) {
	clk := &fakeClock{now: at("2026-10-23 09:00")}
	s := &Scheduler{Policy: SchedulePolicy{Schedule: MustParseSchedule("0 2 * * *"), Missed: MissedRunOnce}, Clock: clk, Last: at("2026-10-20 02:00")}
	d, _ := s.Tick()
	if !d.Run {
		t.Fatal("no catch-up run")
	}
	d, _ = s.Tick()
	if d.Run {
		t.Fatal("second catch-up run (burst)")
	}
	clk.now = at("2026-10-24 02:01")
	if d, _ = s.Tick(); !d.Run || d.Missed != 0 {
		t.Fatalf("next regular run: %+v", d)
	}
	// Skip policy also advances past skipped times.
	sk := &Scheduler{Policy: SchedulePolicy{Schedule: MustParseSchedule("0 2 * * *"), Missed: MissedSkip}, Clock: &fakeClock{now: at("2026-10-23 09:00")}, Last: at("2026-10-20 02:00")}
	if d, _ := sk.Tick(); d.Run || !sk.Last.Equal(at("2026-10-23 02:00")) {
		t.Fatalf("skip tick %+v last %s", d, sk.Last)
	}
}
