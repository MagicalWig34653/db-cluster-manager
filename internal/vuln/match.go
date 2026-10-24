// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"slices"
	"sort"
	"time"
)

// Match is the deterministic matcher: it decides from ecosystem, package
// name and version ranges alone whether an advisory affects a component.
// It never uses AI, so findings are reproducible without a provider.
// Withdrawn advisories match nothing. now stamps FirstSeen/LastSeen.
func Match(advisories []Advisory, inventory []InventoryEntry, now time.Time) []Finding {
	type key struct{ eco, pkg string }
	index := map[key][]int{}
	for i, a := range advisories {
		if a.Withdrawn {
			continue
		}
		for _, af := range a.Affect {
			k := key{af.Ecosystem, af.Package}
			if !slices.Contains(index[k], i) {
				index[k] = append(index[k], i)
			}
		}
	}
	var out []Finding
	for _, e := range inventory {
		stateful := e.ServiceKind.Stateful()
		for _, c := range e.components() {
			c.Ecosystem = NormalizeEcosystem(c.Ecosystem)
			c.Name = NormalizePackage(c.Name)
			for _, i := range index[key{c.Ecosystem, c.Name}] {
				a := advisories[i]
				hit := false
				for _, af := range a.Affect {
					if af.Ecosystem == c.Ecosystem && af.Package == c.Name && af.Affects(c.Version) {
						hit = true
						break
					}
				}
				if !hit {
					continue
				}
				out = append(out, Finding{
					ID: FindingID(a.ID, e.ID, c), AdvisoryID: a.ID, Aliases: slices.Clone(a.Aliases),
					EntryID: e.ID, Image: e.Image, Services: sortedUnique(e.Services), Kind: e.ServiceKind,
					Coverage: e.Coverage, Component: c, FixedVersions: a.FixedVersions(c.Ecosystem, c.Name),
					Severity: a.Severity, CVSS: a.CVSS, KEV: a.KEV, EPSS: a.EPSS,
					Prioritization: Prioritize(a.Severity, a.CVSS, a.KEV, a.EPSS, stateful),
					Status:         StatusAffected, FirstSeen: now, LastSeen: now, Sources: slices.Clone(a.Sources),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Reconcile merges freshly matched findings into the stored ones: a known
// finding keeps its status, decision and FirstSeen and gets fresh
// prioritisation; a stored open finding that no longer matches is marked
// fixed (the component changed or the advisory was withdrawn); a
// not_affected or fixed finding never re-opens by itself. It returns the
// findings to store and the IDs that are new (to notify).
func Reconcile(stored, fresh []Finding, now time.Time) (changed []Finding, created []string) {
	old := make(map[string]Finding, len(stored))
	for _, f := range stored {
		old[f.ID] = f
	}
	seen := map[string]bool{}
	for _, f := range fresh {
		seen[f.ID] = true
		prev, ok := old[f.ID]
		if !ok {
			changed = append(changed, f)
			created = append(created, f.ID)
			continue
		}
		f.FirstSeen, f.Status, f.Justification, f.StatusBy, f.ResolvedAt = prev.FirstSeen, prev.Status, prev.Justification, prev.StatusBy, prev.ResolvedAt
		if prev.Status == StatusFixed && !prev.ResolvedAt.IsZero() && prev.StatusBy == "" {
			// Auto-resolved earlier and matching again (for example a
			// rollback to the old image): re-open.
			f.Status, f.ResolvedAt = StatusAffected, time.Time{}
			created = append(created, f.ID)
		}
		changed = append(changed, f)
	}
	for _, f := range stored {
		if seen[f.ID] || !f.Status.Open() {
			continue
		}
		f.Status, f.ResolvedAt, f.StatusBy, f.Justification = StatusFixed, now, "", "no longer matched by the inventory"
		changed = append(changed, f)
	}
	return changed, created
}
