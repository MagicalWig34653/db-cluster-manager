// SPDX-License-Identifier: AGPL-3.0-only

// Package routing contains the pure decision logic of the database proxy: which
// node is the writer, which nodes serve reads, and how the old writer is drained
// when the writer changes. It performs no I/O, starts no goroutines and reads no
// clock; callers pass "now" and a snapshot of node state to Router.Decide.
//
// Rules implemented here:
//
//   - Unknown is not healthy. A node is usable only if it reports Healthy, has a
//     LastSeen no older than Policy.StaleAfter (and not implausibly far in the
//     future) and is in SyncSynced. Missing reports are stale, never healthy.
//   - Only nodes with RoleWriterEligible can become writer. RoleStandby and
//     RoleExcluded nodes are never selected for writes or reads.
//   - Latency is smoothed with an EWMA over the supplied samples.
//   - Hysteresis (Policy.SwitchMargin and Policy.SwitchConsecutive) only delays
//     optional switches between two healthy nodes. A writer that became unusable
//     is replaced immediately, and if no node qualifies the decision explicitly
//     reports "no writer".
//   - When the writer changes, the decision carries a DrainPlan for the previous
//     writer: accept no new connections, existing ones get a deadline.
//
// Limits (not handled here): read/write splitting correctness. This package
// only ranks endpoints. Keeping transactions on one endpoint, honouring
// read-after-write expectations, and routing statements inside a session are
// the caller's responsibility and must be enforced by it (for example by
// pinning sessions or only sending explicitly read-only work to reader
// endpoints). Replication lag is represented only coarsely by SyncState.
//
// A Router is stateful (current writer, hysteresis streak) and not safe for
// concurrent use; the caller must serialise calls to Decide.
package routing

import (
	"fmt"
	"sort"
	"time"
)

// Role is the operator-configured purpose of a node.
type Role int

const (
	// RoleExcluded nodes are never used. This is the zero value so that a
	// forgotten Role fails closed.
	RoleExcluded Role = iota
	// RoleWriterEligible nodes may be writer and may serve reads.
	RoleWriterEligible
	// RoleReader nodes serve reads only.
	RoleReader
	// RoleStandby nodes are kept as members but receive no traffic.
	RoleStandby
)

func (r Role) String() string {
	switch r {
	case RoleExcluded:
		return "excluded"
	case RoleWriterEligible:
		return "writer-eligible"
	case RoleReader:
		return "reader"
	case RoleStandby:
		return "standby"
	}
	return fmt.Sprintf("role(%d)", int(r))
}

// SyncState is the replication state reported for a node.
type SyncState int

const (
	// SyncUnknown is the zero value; it is not usable.
	SyncUnknown SyncState = iota
	// SyncSynced means the node is a fully synced cluster member.
	SyncSynced
	// SyncDonor means the node is serving state transfer to a joiner.
	SyncDonor
	// SyncJoining means the node is still receiving state.
	SyncJoining
	// SyncDesynced means the node is not applying cluster writes.
	SyncDesynced
)

func (s SyncState) String() string {
	switch s {
	case SyncUnknown:
		return "unknown"
	case SyncSynced:
		return "synced"
	case SyncDonor:
		return "donor"
	case SyncJoining:
		return "joining"
	case SyncDesynced:
		return "desynced"
	}
	return fmt.Sprintf("sync(%d)", int(s))
}

// Node is a snapshot of one database node as seen by this proxy.
type Node struct {
	ID       string
	Healthy  bool
	LastSeen time.Time // time of the last health report; zero means never
	Role     Role
	Sync     SyncState
	// LatencySamples are locally measured latencies, oldest first. Samples
	// <= 0 are ignored. No usable samples means the latency is unknown and the
	// node ranks after all nodes with known latency.
	LatencySamples []time.Duration
}

// WriterMode selects how the writer is chosen.
type WriterMode int

const (
	// WriterStable keeps the current writer while it is usable and picks the
	// lowest-latency node only when there is no usable current writer.
	WriterStable WriterMode = iota
	// WriterLowestLatency moves to the lowest-latency node once it is
	// better by the configured margin for enough consecutive evaluations.
	WriterLowestLatency
	// WriterPreferNode uses Policy.PreferredID whenever it is usable (moving
	// back to it only after SwitchConsecutive evaluations) and otherwise
	// behaves like WriterStable.
	WriterPreferNode
)

func (m WriterMode) String() string {
	switch m {
	case WriterStable:
		return "single-writer-stable"
	case WriterLowestLatency:
		return "lowest-latency"
	case WriterPreferNode:
		return "prefer-node"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// ReadMode selects the optional read endpoint list.
type ReadMode int

const (
	// ReadDisabled returns no read endpoints (default).
	ReadDisabled ReadMode = iota
	// ReadExcludeWriter lists usable nodes except the writer.
	ReadExcludeWriter
	// ReadIncludeWriter lists all usable nodes including the writer.
	ReadIncludeWriter
	// ReadFallbackToWriter lists usable nodes except the writer, but the
	// writer alone if there is no other usable node.
	ReadFallbackToWriter
)

// Policy configures the router. Zero fields take the documented defaults, see
// DefaultPolicy.
type Policy struct {
	// Mode is the writer policy. Default WriterStable.
	Mode WriterMode
	// PreferredID is used by WriterPreferNode.
	PreferredID string
	// Read selects the read endpoint list. Default ReadDisabled.
	Read ReadMode
	// StaleAfter: a node without a health report for longer is unhealthy.
	// Default 15s.
	StaleAfter time.Duration
	// EWMAAlpha in (0,1] is the weight of the newest sample. Default 0.3.
	EWMAAlpha float64
	// SwitchMargin: a candidate's smoothed latency must be lower than
	// current*(1-SwitchMargin) to count towards a switch. Default 0.2 (20%).
	SwitchMargin float64
	// SwitchConsecutive is the number of consecutive evaluations a candidate
	// must qualify before an optional switch happens. Default 3.
	SwitchConsecutive int
	// DrainDeadline is how long existing connections to the old writer may
	// continue after a writer change. Default 30s.
	DrainDeadline time.Duration
}

// Defaults.
const (
	DefaultStaleAfter        = 15 * time.Second
	DefaultEWMAAlpha         = 0.3
	DefaultSwitchMargin      = 0.2
	DefaultSwitchConsecutive = 3
	DefaultDrainDeadline     = 30 * time.Second
)

// DefaultPolicy returns the policy with all defaults filled in.
func DefaultPolicy() Policy { return Policy{}.withDefaults() }

func (p Policy) withDefaults() Policy {
	if p.StaleAfter <= 0 {
		p.StaleAfter = DefaultStaleAfter
	}
	if p.EWMAAlpha <= 0 || p.EWMAAlpha > 1 {
		p.EWMAAlpha = DefaultEWMAAlpha
	}
	if p.SwitchMargin <= 0 || p.SwitchMargin >= 1 {
		p.SwitchMargin = DefaultSwitchMargin
	}
	if p.SwitchConsecutive <= 0 {
		p.SwitchConsecutive = DefaultSwitchConsecutive
	}
	if p.DrainDeadline <= 0 {
		p.DrainDeadline = DefaultDrainDeadline
	}
	return p
}

// DrainPlan describes how to retire the previous writer.
type DrainPlan struct {
	NodeID string
	// AcceptNew is always false: no new connections go to the node.
	AcceptNew bool
	// Deadline is when remaining connections should be closed.
	Deadline time.Time
}

// Decision is the result of one evaluation.
type Decision struct {
	// Writer is the chosen writer; empty when HasWriter is false.
	Writer string
	// HasWriter is false for the explicit "no writer" decision: callers must
	// refuse new write connections rather than guess.
	HasWriter bool
	// Changed reports a change of writer relative to the previous decision.
	Changed bool
	// Previous is the writer of the previous decision (may be empty).
	Previous string
	// Drain is set when Changed and there was a previous writer.
	Drain *DrainPlan
	// Readers are the read endpoints ordered best first; see ReadMode.
	Readers []string
	// Reasons explain the decision in order, for logs and the UI.
	Reasons []string
}

// Router holds the hysteresis state between evaluations.
type Router struct {
	policy Policy

	current   string
	candidate string
	streak    int
}

// New returns a Router for policy (defaults applied to zero fields).
func New(policy Policy) *Router { return &Router{policy: policy.withDefaults()} }

// Policy returns the effective policy.
func (r *Router) Policy() Policy { return r.policy }

// SetPolicy replaces the policy (defaults applied to zero fields) and keeps
// the current writer, so a configuration reload does not by itself move the
// writer. The hysteresis streak is reset because it was measured under the
// old policy.
func (r *Router) SetPolicy(policy Policy) {
	r.policy = policy.withDefaults()
	r.resetStreak()
}

// Current returns the current writer ID, empty if none.
func (r *Router) Current() string { return r.current }

// EWMA returns the exponentially weighted moving average of samples, oldest
// first, and whether any positive sample existed.
func EWMA(samples []time.Duration, alpha float64) (time.Duration, bool) {
	var s float64
	have := false
	for _, x := range samples {
		if x <= 0 {
			continue
		}
		if !have {
			s, have = float64(x), true
			continue
		}
		s = alpha*float64(x) + (1-alpha)*s
	}
	return time.Duration(s), have
}

type scored struct {
	id      string
	latency time.Duration
	known   bool
}

// better orders by known latency, then ID for determinism.
func (a scored) better(b scored) bool {
	if a.known != b.known {
		return a.known
	}
	if a.latency != b.latency {
		return a.latency < b.latency
	}
	return a.id < b.id
}

// usable reports whether n may receive traffic at now and why not.
func (p Policy) usable(n Node, now time.Time) (bool, string) {
	switch n.Role {
	case RoleWriterEligible, RoleReader:
	default:
		return false, fmt.Sprintf("%s: role %s", n.ID, n.Role)
	}
	if n.LastSeen.IsZero() {
		return false, fmt.Sprintf("%s: never reported, unknown is not healthy", n.ID)
	}
	if age := now.Sub(n.LastSeen); age > p.StaleAfter || age < -p.StaleAfter {
		return false, fmt.Sprintf("%s: health report stale (age %s)", n.ID, age)
	}
	if !n.Healthy {
		return false, fmt.Sprintf("%s: unhealthy", n.ID)
	}
	if n.Sync != SyncSynced {
		return false, fmt.Sprintf("%s: sync state %s", n.ID, n.Sync)
	}
	return true, ""
}

// Decide evaluates the nodes at now and returns the routing decision. It
// updates hysteresis state; call it once per evaluation round.
func (r *Router) Decide(now time.Time, nodes []Node) Decision {
	p := r.policy
	var d Decision
	d.Previous = r.current

	var all, writers []scored
	for _, n := range nodes {
		ok, why := p.usable(n, now)
		if !ok {
			d.Reasons = append(d.Reasons, why)
			continue
		}
		lat, known := EWMA(n.LatencySamples, p.EWMAAlpha)
		s := scored{id: n.ID, latency: lat, known: known}
		all = append(all, s)
		if n.Role == RoleWriterEligible {
			writers = append(writers, s)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].better(all[j]) })
	sort.Slice(writers, func(i, j int) bool { return writers[i].better(writers[j]) })
	find := func(id string) (scored, bool) {
		for _, s := range writers {
			if s.id == id {
				return s, true
			}
		}
		return scored{}, false
	}

	newWriter := ""
	if len(writers) == 0 {
		d.Reasons = append(d.Reasons, "no usable writer-eligible node: no writer")
		r.resetStreak()
	} else if cur, ok := find(r.current); ok {
		newWriter = r.pickWithHysteresis(&d, cur, writers[0], find)
	} else {
		newWriter = r.pickImmediate(&d, find, writers[0])
	}

	d.Writer = newWriter
	d.HasWriter = newWriter != ""
	d.Changed = newWriter != r.current
	if d.Changed {
		if r.current != "" {
			d.Drain = &DrainPlan{NodeID: r.current, AcceptNew: false, Deadline: now.Add(p.DrainDeadline)}
			d.Reasons = append(d.Reasons, fmt.Sprintf("draining %s until %s", r.current, d.Drain.Deadline.Format(time.RFC3339)))
		}
		r.resetStreak()
	}
	r.current = newWriter
	d.Readers = r.readers(all, newWriter, &d)
	return d
}

func (r *Router) resetStreak() { r.candidate, r.streak = "", 0 }

// pickImmediate selects a writer when there is no usable current writer.
// Hysteresis never delays this.
func (r *Router) pickImmediate(d *Decision, find func(string) (scored, bool), best scored) string {
	p := r.policy
	why := "no current writer"
	if r.current != "" {
		why = fmt.Sprintf("current writer %s not usable", r.current)
	}
	if p.Mode == WriterPreferNode {
		if pref, ok := find(p.PreferredID); ok {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s: immediate choice of preferred node %s", why, pref.id))
			return pref.id
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf("preferred node %q not usable", p.PreferredID))
	}
	d.Reasons = append(d.Reasons, fmt.Sprintf("%s: immediate choice of lowest latency node %s", why, best.id))
	return best.id
}

// pickWithHysteresis handles a usable current writer: optional switches need a
// margin (latency mode) and consecutive confirmations.
func (r *Router) pickWithHysteresis(d *Decision, cur, best scored, find func(string) (scored, bool)) string {
	p := r.policy
	var target string
	qualifies := false

	switch p.Mode {
	case WriterPreferNode:
		if pref, ok := find(p.PreferredID); ok && pref.id != cur.id {
			target, qualifies = pref.id, true
		}
	case WriterLowestLatency:
		if best.id != cur.id && best.known && (!cur.known ||
			float64(best.latency) < float64(cur.latency)*(1-p.SwitchMargin)) {
			target, qualifies = best.id, true
		}
	}

	if !qualifies {
		if r.streak > 0 {
			d.Reasons = append(d.Reasons, fmt.Sprintf("switch candidate %s no longer qualifies; streak reset", r.candidate))
		}
		r.resetStreak()
		d.Reasons = append(d.Reasons, fmt.Sprintf("keeping writer %s", cur.id))
		return cur.id
	}
	if r.candidate == target {
		r.streak++
	} else {
		r.candidate, r.streak = target, 1
	}
	if r.streak >= p.SwitchConsecutive {
		d.Reasons = append(d.Reasons, fmt.Sprintf("switching %s -> %s after %d consecutive evaluations", cur.id, target, r.streak))
		return target
	}
	d.Reasons = append(d.Reasons, fmt.Sprintf("keeping writer %s; candidate %s qualified %d/%d", cur.id, target, r.streak, p.SwitchConsecutive))
	return cur.id
}

func (r *Router) readers(all []scored, writer string, d *Decision) []string {
	var out []string
	switch r.policy.Read {
	case ReadDisabled:
		return nil
	case ReadIncludeWriter:
		for _, s := range all {
			out = append(out, s.id)
		}
	case ReadExcludeWriter, ReadFallbackToWriter:
		for _, s := range all {
			if s.id != writer {
				out = append(out, s.id)
			}
		}
		if len(out) == 0 && r.policy.Read == ReadFallbackToWriter && writer != "" {
			out = append(out, writer)
			d.Reasons = append(d.Reasons, "no other read node: reads fall back to the writer")
		}
	}
	if len(out) == 0 {
		d.Reasons = append(d.Reasons, "no read endpoint available")
	}
	return out
}
