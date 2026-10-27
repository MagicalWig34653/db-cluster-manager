// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"sort"
	"time"
)

// Diff is the change between two reports of one host.
type Diff struct {
	NewUpdates       []Update  `json:"newUpdates,omitempty"`
	NewSecurity      []Update  `json:"newSecurity,omitempty"`
	ClearedUpdates   []string  `json:"clearedUpdates,omitempty"`
	InstalledChanged []Change  `json:"installedChanged,omitempty"`
	Installed        []Package `json:"installedNew,omitempty"`
	Removed          []string  `json:"removed,omitempty"`
	RebootChanged    bool      `json:"rebootChanged,omitempty"`
	KernelChanged    bool      `json:"kernelChanged,omitempty"`
}

// Empty reports whether nothing changed.
func (d Diff) Empty() bool {
	return len(d.NewUpdates) == 0 && len(d.ClearedUpdates) == 0 && len(d.InstalledChanged) == 0 &&
		len(d.Installed) == 0 && len(d.Removed) == 0 && !d.RebootChanged && !d.KernelChanged
}

func pkgKey(name, arch string) string { return name + "\x00" + arch }

// DiffReports compares prev (may be nil) with cur. An update counts as new
// when its (name, available version) pair was not pending before, so a new
// version of an already pending package alerts again.
func DiffReports(prev *HostReport, cur HostReport) Diff {
	var d Diff
	if prev == nil {
		for _, u := range cur.Updates {
			d.NewUpdates = append(d.NewUpdates, u)
			if u.Security {
				d.NewSecurity = append(d.NewSecurity, u)
			}
		}
		return d
	}
	had := map[string]bool{}
	for _, u := range prev.Updates {
		had[u.Name+"\x00"+u.Available] = true
	}
	now := map[string]bool{}
	for _, u := range cur.Updates {
		now[u.Name] = true
		if !had[u.Name+"\x00"+u.Available] {
			d.NewUpdates = append(d.NewUpdates, u)
			if u.Security {
				d.NewSecurity = append(d.NewSecurity, u)
			}
		}
	}
	for _, u := range prev.Updates {
		if !now[u.Name] {
			d.ClearedUpdates = append(d.ClearedUpdates, u.Name)
		}
	}
	old := map[string]string{}
	for _, p := range prev.Installed {
		old[pkgKey(p.Name, p.Arch)] = p.Version
	}
	cn := map[string]bool{}
	for _, p := range cur.Installed {
		k := pkgKey(p.Name, p.Arch)
		cn[k] = true
		v, ok := old[k]
		switch {
		case !ok:
			d.Installed = append(d.Installed, p)
		case v != p.Version:
			d.InstalledChanged = append(d.InstalledChanged, Change{Name: p.Name, From: v, To: p.Version})
		}
	}
	for _, p := range prev.Installed {
		if !cn[pkgKey(p.Name, p.Arch)] {
			d.Removed = append(d.Removed, p.Name)
		}
	}
	sort.Strings(d.ClearedUpdates)
	sort.Strings(d.Removed)
	d.RebootChanged = prev.RebootRequired != cur.RebootRequired
	d.KernelChanged = prev.OS.Kernel != cur.OS.Kernel
	return d
}

// HistoryEntry is one stored change of a host. Full installed lists are not
// kept in the history, only the diff and counters.
type HistoryEntry struct {
	HostID         string    `json:"hostId"`
	At             time.Time `json:"at"`
	Kind           string    `json:"kind"` // report | apply | reboot
	PlanID         string    `json:"planId,omitempty"`
	Result         string    `json:"result,omitempty"`
	Diff           Diff      `json:"diff"`
	UpdatesPending int       `json:"updatesPending"`
	SecurityCount  int       `json:"securityPending"`
	RebootRequired Tristate  `json:"rebootRequired"`
}
