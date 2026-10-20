// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"sort"
	"time"
)

// Entry is a stored backup as seen by retention.
type Entry struct {
	Name    string
	Created time.Time
}

// Policy keeps the union of: the newest KeepLast backups, the newest backup
// of each of the KeepDaily most recent UTC days that have backups, and the
// newest backup of each of the KeepWeekly most recent ISO weeks (UTC) that
// have backups. Zero disables a rule; an all-zero policy keeps everything.
type Policy struct {
	KeepLast   int
	KeepDaily  int
	KeepWeekly int
}

// Select splits entries into those to keep and those to remove. It is pure
// and does not modify its input.
func Select(entries []Entry, p Policy) (keep, remove []Entry) {
	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Created.Equal(sorted[j].Created) {
			return sorted[i].Created.After(sorted[j].Created)
		}
		return sorted[i].Name > sorted[j].Name
	})
	if p == (Policy{}) {
		return sorted, nil
	}
	kept := make(map[string]bool)
	for i := 0; i < len(sorted) && i < p.KeepLast; i++ {
		kept[sorted[i].Name] = true
	}
	days := map[[3]int]bool{}
	weeks := map[[2]int]bool{}
	for _, e := range sorted {
		t := e.Created.UTC()
		y, m, d := t.Date()
		dk := [3]int{y, int(m), d}
		if !days[dk] && len(days) < p.KeepDaily {
			days[dk] = true
			kept[e.Name] = true
		}
		wy, ww := t.ISOWeek()
		wk := [2]int{wy, ww}
		if !weeks[wk] && len(weeks) < p.KeepWeekly {
			weeks[wk] = true
			kept[e.Name] = true
		}
	}
	for _, e := range sorted {
		if kept[e.Name] {
			keep = append(keep, e)
		} else {
			remove = append(remove, e)
		}
	}
	return keep, remove
}
