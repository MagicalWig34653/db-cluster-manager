// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrRequiresGuidedRecovery reports that automatic single-node
	// replacement is not safe: quorum or a verified donor is missing. The
	// cluster must go through guided recovery with explicit approval (A06).
	// Callers must never retry the replacement with relaxed inputs.
	ErrRequiresGuidedRecovery = errors.New("galera: automatic replacement not safe; guided recovery required")
	// ErrNodeNotFailed reports a replacement request for a node that is a
	// live member of the Primary component.
	ErrNodeNotFailed = errors.New("galera: node is not failed")
	// ErrUnknownNode reports a slot that is not part of the cluster state.
	ErrUnknownNode = errors.New("galera: unknown node slot")
	// ErrReplacementPaused reports a failed node on a host in maintenance
	// (A08: maintenance pauses replacement for the host).
	ErrReplacementPaused = errors.New("galera: replacement paused for host in maintenance")
)

// GuidedRecoveryError carries the reasons why automatic replacement was
// refused. It matches ErrRequiresGuidedRecovery with errors.Is.
type GuidedRecoveryError struct {
	Slot    string
	Reasons []string
}

func (e *GuidedRecoveryError) Error() string {
	return fmt.Sprintf("%v (slot %s: %s)", ErrRequiresGuidedRecovery, e.Slot, strings.Join(e.Reasons, "; "))
}

func (e *GuidedRecoveryError) Unwrap() error { return ErrRequiresGuidedRecovery }

// DefaultMaxObservationAge bounds how old a health observation may be to
// count toward quorum. Older observations are treated as Unknown.
const DefaultMaxObservationAge = 30 * time.Second

// NodeState is a node with its last observed health.
type NodeState struct {
	Node
	Health     NodeHealth `json:"health"`
	ObservedAt time.Time  `json:"observed_at"`
}

// ClusterState is the manager's view of a running cluster.
type ClusterState struct {
	Cluster Cluster     `json:"cluster"`
	Nodes   []NodeState `json:"nodes"`
	// LastKnownSize is the size of the last Primary component the manager
	// recorded (normally the configured size). Quorum is a strict majority
	// of max(LastKnownSize, len(Nodes)); 0 means unknown and blocks
	// automatic replacement.
	LastKnownSize int    `json:"last_known_size"`
	Hosts         []Host `json:"hosts"`
}

// ReplacementPolicy configures PlanReplacement.
type ReplacementPolicy struct {
	Placement PlacementPolicy
	// Now is the evaluation time for observation freshness.
	Now time.Time
	// MaxObservationAge defaults to DefaultMaxObservationAge.
	MaxObservationAge time.Duration
	// PausedHosts are hosts in guided maintenance (A08).
	PausedHosts []string
	// NewUUID generates the replacement's node UUID; nil is random.
	NewUUID func() (string, error)
}

// fresh returns the health if observed within the allowed age, else Unknown.
func (p ReplacementPolicy) fresh(n NodeState) NodeHealth {
	age := p.MaxObservationAge
	if age <= 0 {
		age = DefaultMaxObservationAge
	}
	d := p.Now.Sub(n.ObservedAt)
	if n.ObservedAt.IsZero() || d < 0 || d > age {
		return NodeHealth{State: HealthUnknown, Reasons: []string{"observation missing, stale or in the future"}}
	}
	return n.Health
}

// PlanReplacement plans the automatic replacement of failedSlot (A02,
// ADR 0004 item 4). The replacement gets a NEW identity: a new node UUID, a
// new volume and a new pinned service in the same slot (same service and DNS
// name, so the other members' cluster address stays valid). The old
// service is removed; the old volume is retained for inspection and never
// deleted by this plan.
//
// It refuses with *GuidedRecoveryError (ErrRequiresGuidedRecovery) unless
// all of the following hold on fresh observations:
//
//   - the last known cluster size is known;
//   - at least a strict majority of it, excluding the failed node, are
//     connected and ready members of the Primary component;
//   - all of them report the same cluster state UUID and a cluster size of
//     at least that majority;
//   - at least one of them is Synced and healthy (the verified donor).
//
// It never relaxes these rules; a lack of capacity is ErrNoCapacity.
func PlanReplacement(st ClusterState, failedSlot string, pol ReplacementPolicy) (Plan, error) {
	if pol.Now.IsZero() {
		return Plan{}, fmt.Errorf("%w: evaluation time required", ErrInvalidTopology)
	}
	fi := slices.IndexFunc(st.Nodes, func(n NodeState) bool { return n.Slot == failedSlot })
	if fi < 0 {
		return Plan{}, fmt.Errorf("%w: %s", ErrUnknownNode, failedSlot)
	}
	failed := st.Nodes[fi]
	if pol.fresh(failed).InPrimary() {
		return Plan{}, fmt.Errorf("%w: %s is a live Primary member", ErrNodeNotFailed, failedSlot)
	}
	if slices.Contains(pol.PausedHosts, failed.HostID) {
		return Plan{}, fmt.Errorf("%w: %s on host %s", ErrReplacementPaused, failedSlot, failed.HostID)
	}

	size := max(st.LastKnownSize, len(st.Nodes))
	majority := size/2 + 1
	var reasons []string
	if st.LastKnownSize <= 0 {
		reasons = append(reasons, "last known cluster size unknown")
	}
	var members []NodeState
	var notLive []string // explain a missing majority; alone they do not block
	stateUUIDs := map[string]bool{}
	for _, n := range st.Nodes {
		if n.Slot == failedSlot {
			continue
		}
		h := pol.fresh(n)
		if !h.InPrimary() {
			notLive = append(notLive, fmt.Sprintf("%s not a live Primary member (%s)", n.Slot, h.State))
			continue
		}
		if h.ClusterSize < majority {
			reasons = append(reasons, fmt.Sprintf("%s reports cluster size %d below majority %d", n.Slot, h.ClusterSize, majority))
		}
		stateUUIDs[h.StateUUID] = true
		members = append(members, n)
	}
	if len(members) < majority {
		reasons = append(reasons, fmt.Sprintf("%d live Primary members, need a majority of %d of last known size %d", len(members), majority, size))
		reasons = append(reasons, notLive...)
	}
	if len(stateUUIDs) > 1 || stateUUIDs[""] {
		reasons = append(reasons, "members report diverging or missing cluster state UUIDs")
	}
	di := slices.IndexFunc(members, func(n NodeState) bool { return pol.fresh(n).DonorCapable() })
	if di < 0 {
		reasons = append(reasons, "no verified Synced donor")
	}
	if len(reasons) > 0 {
		return Plan{}, &GuidedRecoveryError{Slot: failedSlot, Reasons: reasons}
	}
	donor := members[di]
	donorSize := pol.fresh(donor).ClusterSize

	warns, err := pol.Placement.validate(st.Hosts)
	if err != nil {
		return Plan{}, err
	}
	pl := newPlacer(st.Hosts, pol.Placement, forReplace)
	var existing []string
	for _, n := range st.Nodes {
		pl.used[n.HostID] = true
		existing = append(existing, n.NodeUUID)
	}
	h, err := pl.choose(failedSlot, failed.Resources.Demand())
	if err != nil {
		var pe *PlacementError
		if errors.As(err, &pe) {
			pe.Rejected[failed.HostID] = "host of the failed node; its old volume needs reconciliation first"
		}
		return Plan{}, err
	}
	u, err := newUUIDSource(pol.NewUUID, existing...).next()
	if err != nil {
		return Plan{}, err
	}
	nn := Node{Slot: failedSlot, NodeUUID: u, HostID: h.ID, Volume: VolumeName(st.Cluster.Name, failedSlot, u), Resources: failed.Resources}
	spec, err := BuildServiceSpec(st.Cluster, nn, SpecOptions{SSTDonor: donor.Slot})
	if err != nil {
		return Plan{}, err
	}

	nodes := make([]Node, len(st.Nodes))
	desired := make([]ServiceSpec, len(st.Nodes))
	for i, n := range st.Nodes {
		nodes[i] = n.Node
		if i == fi {
			nodes[i] = nn
			desired[i] = spec
			continue
		}
		d, err := BuildServiceSpec(st.Cluster, n.Node, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		desired[i] = d
	}

	quorum := Precondition{Kind: PreQuorum, ClusterSize: majority}
	var l stepList
	l.add(Step{Kind: StepRemoveService, Slot: failedSlot, HostID: failed.HostID, NodeUUID: failed.NodeUUID,
		Note: "Swarm keeps a pinned task Pending on a lost host; removal prevents a later unreconciled restart"}, quorum)
	l.add(Step{Kind: StepRetainVolume, Slot: failedSlot, HostID: failed.HostID, NodeUUID: failed.NodeUUID, Volume: failed.Volume,
		Note: "old volume kept for inspection; deletion only after explicit user approval"})
	l.addNewVolume(st.Cluster, nn)
	l.add(Step{Kind: StepVerifyDonor, Slot: donor.Slot, HostID: donor.HostID, ClusterSize: donorSize}, synced(donor.Slot, donorSize))
	l.add(Step{Kind: StepCreateService, Slot: failedSlot, HostID: nn.HostID, NodeUUID: nn.NodeUUID, Spec: specPtr(spec),
		Note: "joins with a new identity; full state transfer from " + donor.Slot}, quorum, synced(donor.Slot, donorSize))
	l.add(Step{Kind: StepWaitSynced, Slot: failedSlot, HostID: nn.HostID, ClusterSize: donorSize + 1})

	return Plan{
		Kind: PlanReplaceNode, ClusterID: st.Cluster.ClusterID, Cluster: st.Cluster,
		Nodes: nodes, Desired: desired, Steps: l.steps, Warnings: warns,
	}, nil
}
