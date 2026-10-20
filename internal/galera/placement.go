// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrNoCapacity reports that no eligible host can take a node.
var ErrNoCapacity = errors.New("galera: no eligible host with sufficient capacity")

// ErrPolicyConflict reports a contradictory placement policy.
var ErrPolicyConflict = errors.New("galera: contradictory placement policy")

// Host is a Swarm node as seen by the planner.
type Host struct {
	ID     string `json:"id"` // Swarm node ID
	Name   string `json:"name,omitempty"`
	Ready  bool   `json:"ready"`  // Swarm node status ready
	Active bool   `json:"active"` // availability active (not drained/paused)
	// Spare hosts are kept free for replacement (A02/A03). Cluster creation
	// never uses them.
	Spare bool `json:"spare,omitempty"`
	// Capacity is the host total. Zero means unknown and the host is
	// rejected.
	Capacity ResourceAmount `json:"capacity"`
	// Allocated is the demand already committed on the host by managed
	// workloads (other database nodes, agents, proxies).
	Allocated ResourceAmount `json:"allocated"`
}

// PlacementPolicy implements the placement modes of A01. All lists hold host
// IDs. Precedence, strongest first:
//
//  1. Allowlist: if non-empty, the strict set of candidate hosts. No host
//     outside it is ever used, not even a preferred one, and there is no
//     fallback when it lacks capacity.
//  2. Exclusion: excluded hosts are never used, not even preferred ones.
//     A host that is both allowlisted and excluded is a contradiction and is
//     rejected with ErrPolicyConflict instead of being resolved silently.
//  3. Preference: eligible preferred hosts are chosen before all other
//     hosts, in list order. A preferred host that is excluded or outside
//     the allowlist is ignored with a warning.
//  4. Automatic scoring among the remaining eligible hosts: most free memory
//     after placement, then most free CPU, then host ID (deterministic).
//
// Hard rules apply in every mode: the host must be ready and active, must
// not already host a node of the same cluster, must not be a spare during
// creation, and must have the node's demand free after Reserve.
type PlacementPolicy struct {
	Allowlist []string `json:"allowlist,omitempty"`
	Excluded  []string `json:"excluded,omitempty"`
	Preferred []string `json:"preferred,omitempty"`
	// Reserve is kept free on every host for unrelated workloads (A04).
	Reserve ResourceAmount `json:"reserve"`
}

// validate checks for contradictions and returns warnings for entries that
// have no effect.
func (p PlacementPolicy) validate(hosts []Host) ([]string, error) {
	if p.Reserve.MilliCPU < 0 || p.Reserve.MemoryBytes < 0 {
		return nil, fmt.Errorf("%w: negative reserve", ErrPolicyConflict)
	}
	known := map[string]bool{}
	for _, h := range hosts {
		known[h.ID] = true
	}
	var warns []string
	for _, id := range slices.Concat(p.Allowlist, p.Excluded, p.Preferred) {
		if !known[id] {
			warns = append(warns, "unknown host "+id+" in placement policy")
		}
	}
	for _, id := range p.Allowlist {
		if slices.Contains(p.Excluded, id) {
			return nil, fmt.Errorf("%w: host %s is both allowlisted and excluded", ErrPolicyConflict, id)
		}
	}
	for _, id := range p.Preferred {
		switch {
		case slices.Contains(p.Excluded, id):
			warns = append(warns, "preferred host "+id+" is excluded; preference ignored")
		case len(p.Allowlist) > 0 && !slices.Contains(p.Allowlist, id):
			warns = append(warns, "preferred host "+id+" is not in the allowlist; preference ignored")
		}
	}
	slices.Sort(warns)
	return slices.Compact(warns), nil
}

// PlacementError explains why no host was found for a slot.
type PlacementError struct {
	Slot     string
	Rejected map[string]string // host ID -> reason
}

func (e *PlacementError) Error() string {
	ids := slices.Sorted(maps.Keys(e.Rejected))
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ": " + e.Rejected[id]
	}
	return fmt.Sprintf("%v for slot %s (%s)", ErrNoCapacity, e.Slot, strings.Join(parts, "; "))
}

func (e *PlacementError) Unwrap() error { return ErrNoCapacity }

// placementPurpose distinguishes creation (spares excluded) from
// replacement (spares ranked after preferred hosts).
type placementPurpose int

const (
	forCreate placementPurpose = iota
	forReplace
)

// placer tracks hosts used by the cluster and demand planned so far.
type placer struct {
	hosts   []Host
	policy  PlacementPolicy
	purpose placementPurpose
	used    map[string]bool           // hosts that hold (or will hold) a node of this cluster
	planned map[string]ResourceAmount // additional demand planned per host
}

func newPlacer(hosts []Host, policy PlacementPolicy, purpose placementPurpose) *placer {
	return &placer{hosts: hosts, policy: policy, purpose: purpose, used: map[string]bool{}, planned: map[string]ResourceAmount{}}
}

func (pl *placer) free(h Host) ResourceAmount {
	return h.Capacity.sub(pl.policy.Reserve).sub(h.Allocated).sub(pl.planned[h.ID])
}

// rejection returns why h cannot take demand, or "" if it can.
func (pl *placer) rejection(h Host, demand ResourceAmount) string {
	p := pl.policy
	switch {
	case len(p.Allowlist) > 0 && !slices.Contains(p.Allowlist, h.ID):
		return "not in the allowlist"
	case slices.Contains(p.Excluded, h.ID):
		return "excluded by policy"
	case !h.Ready:
		return "not ready"
	case !h.Active:
		return "not active (drained or paused)"
	case pl.used[h.ID]:
		return "already hosts a node of this cluster"
	case h.Spare && pl.purpose == forCreate:
		return "spare host reserved for replacement"
	case h.Capacity.MilliCPU <= 0 || h.Capacity.MemoryBytes <= 0:
		return "capacity unknown"
	}
	f := pl.free(h)
	if demand.MemoryBytes > f.MemoryBytes {
		return fmt.Sprintf("insufficient memory after reserve: need %d bytes, free %d", demand.MemoryBytes, max(f.MemoryBytes, 0))
	}
	if demand.MilliCPU > f.MilliCPU {
		return fmt.Sprintf("insufficient CPU after reserve: need %d mCPU, free %d", demand.MilliCPU, max(f.MilliCPU, 0))
	}
	return ""
}

// rank is the sort class of an eligible host: lower is better.
func (pl *placer) rank(h Host) int {
	if i := slices.Index(pl.policy.Preferred, h.ID); i >= 0 {
		return i // preferred, in list order
	}
	base := len(pl.policy.Preferred)
	if pl.purpose == forReplace && h.Spare {
		return base
	}
	return base + 1
}

// choose selects the best host for demand and records the placement.
func (pl *placer) choose(slot string, demand ResourceAmount) (Host, error) {
	rejected := map[string]string{}
	var eligible []Host
	for _, h := range pl.hosts {
		if r := pl.rejection(h, demand); r != "" {
			rejected[h.ID] = r
			continue
		}
		eligible = append(eligible, h)
	}
	if len(eligible) == 0 {
		return Host{}, &PlacementError{Slot: slot, Rejected: rejected}
	}
	slices.SortStableFunc(eligible, func(a, b Host) int {
		fa, fb := pl.free(a), pl.free(b)
		return cmp.Or(
			cmp.Compare(pl.rank(a), pl.rank(b)),
			cmp.Compare(fb.MemoryBytes, fa.MemoryBytes),
			cmp.Compare(fb.MilliCPU, fa.MilliCPU),
			cmp.Compare(a.ID, b.ID),
		)
	})
	h := eligible[0]
	pl.used[h.ID] = true
	pl.planned[h.ID] = pl.planned[h.ID].add(demand)
	return h, nil
}
