// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Zero-UUID recovery (ADR 0028).
//
// After a total power loss the surviving Galera component can re-form with
// the zero cluster UUID (lab E9, E12): the group history is lost, every
// grastate.dat holds the zero state and `mariadbd --wsrep-recover` answers
// 0000...:N with the local InnoDB seqno of the node. The ordinary full-outage
// diagnosis has no candidate then, because a zero UUID carries no cluster
// history. The recovery is nevertheless safe to plan: the node with the
// highest local seqno holds the latest commits, bootstraps a NEW state UUID,
// and the others take a full SST (verified by hand in lab E12).

// zeroStateOutage reports that the evidence is the zero-UUID state: no node
// carries a real cluster UUID (neither in grastate.dat nor in a recovered
// position) and at least one carries the zero state. Mixed or diverging
// histories use the ordinary diagnosis, which refuses to propose anything
// across them.
func zeroStateOutage(nodes []NodeInfo) bool {
	zero := false
	for _, n := range nodes {
		if g := n.Grastate; g != nil {
			if g.UUID != ZeroUUID {
				return false
			}
			zero = true
		}
		if r := n.Recovered; r != nil {
			if r.UUID != ZeroUUID {
				return false
			}
			zero = true
		}
	}
	return zero
}

func diagnoseZeroUUID(nodes []NodeInfo) Diagnosis {
	d := Diagnosis{RequiresApproval: true, Kind: PlanZeroUUIDRecovery}
	var known []NodePosition
	for _, n := range nodes {
		p := NodePosition{Slot: n.Slot, UUID: ZeroUUID, Seqno: -1, Source: SourceUnknown}
		switch r := n.Recovered; {
		case r != nil && r.Seqno >= 0:
			p.Seqno, p.Source = r.Seqno, SourceWsrepRecover
			known = append(known, p)
		case n.Grastate == nil && r == nil:
			d.Risks = append(d.Risks, Risk{Kind: RiskMissingNode, Slot: n.Slot, Detail: "no grastate.dat or wsrep-recover result"})
		case r != nil:
			d.Risks = append(d.Risks, Risk{Kind: RiskPositionUnknown, Slot: n.Slot, Detail: "wsrep-recover returned seqno -1"})
		default:
			d.Risks = append(d.Risks, Risk{Kind: RiskPositionUnknown, Slot: n.Slot,
				Detail: "zero-state grastate.dat has no seqno; run mariadbd --wsrep-recover on this node"})
		}
		d.Positions = append(d.Positions, p)
	}
	majority := len(nodes)/2 + 1
	if len(known) < majority {
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"zero cluster UUID state: the zero-UUID recovery is offered, but positions are readable for %d of %d nodes, need %d; run mariadbd --wsrep-recover (POST outage-diagnosis) on the stopped nodes first",
			len(known), len(nodes), majority))
		return d
	}
	slices.SortStableFunc(known, func(a, b NodePosition) int {
		return cmp.Or(cmp.Compare(b.Seqno, a.Seqno), cmp.Compare(a.Slot, b.Slot))
	})
	c := known[0]
	d.Candidate = c.Slot
	d.Reasons = append(d.Reasons,
		fmt.Sprintf("zero cluster UUID state (Galera group history lost): %s has the highest local seqno %d of %d readable positions", c.Slot, c.Seqno, len(known)),
		"the recovery bootstraps that node with a new state UUID and rebuilds the others by full SST; a snapshot of every data directory is taken first")
	unique := true
	var tied []string
	for _, p := range known[1:] {
		if p.Seqno == c.Seqno {
			unique = false
			tied = append(tied, p.Slot)
		}
	}
	if !unique {
		d.Risks = append(d.Risks, Risk{Kind: RiskTie, Detail: fmt.Sprintf(
			"%s ties with %s at local seqno %d; the choice needs a human (lowest slot proposed)", c.Slot, strings.Join(tied, ","), c.Seqno)})
	}
	d.AutoEligible = unique && len(known) == len(nodes)
	if len(known) < len(nodes) {
		d.Reasons = append(d.Reasons, fmt.Sprintf("positions of %d node(s) are unreadable; the candidate may not hold the latest commit", len(nodes)-len(known)))
	}
	d.Plan = recoverySteps(nodes, c, true)
	return d
}

// BuildZeroUUIDPrepare plans the first phase of a zero-UUID recovery: after
// the approval stop every node, wait until no task runs, verify every
// identity marker and snapshot every data directory. Nothing is written to
// the data and nothing is started; the positions are read afterwards, with
// the nodes stopped (a one-off wsrep-recover container needs an unmounted
// volume). The first step waits for the explicit approval.
func BuildZeroUUIDPrepare(c Cluster, nodes []Node) (Plan, error) {
	if len(nodes) < MinClusterSize || len(nodes) != len(c.Members) {
		return Plan{}, fmt.Errorf("%w: %d nodes for %d members", ErrBadRecoveryInput, len(nodes), len(c.Members))
	}
	var l stepList
	for i, n := range nodes {
		var pre []Precondition
		if i == 0 {
			pre = append(pre, Precondition{Kind: PreApproved})
		}
		l.add(Step{Kind: StepStopAll, Slot: n.Slot, Note: "scale the database service to 0 (idempotent)"}, pre...)
	}
	for _, n := range nodes {
		l.add(Step{Kind: StepAwaitStopped, Slot: n.Slot, Note: "no task of the service runs"})
	}
	for _, n := range nodes {
		l.add(Step{Kind: StepVerifyIdentity, Slot: n.Slot})
	}
	for _, n := range nodes {
		l.add(Step{Kind: StepSnapshotData, Slot: n.Slot, Note: "copy the data directory to a snapshot on the same volume before any recovery step"},
			Precondition{Kind: PreNoNodeRunning})
	}
	skeleton := Plan{Kind: PlanZeroUUIDPrepare, Steps: l.steps, RequiresApproval: true}
	return attachPlan(skeleton, "", nodesAsPositions(nodes), c, nodes)
}

func nodesAsPositions(nodes []Node) []NodePosition {
	out := make([]NodePosition, len(nodes))
	for i, n := range nodes {
		out[i] = NodePosition{Slot: n.Slot, Seqno: -1, Source: SourceUnknown}
	}
	return out
}
