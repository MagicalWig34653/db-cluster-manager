// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"slices"
)

// Guided host maintenance (A08).
//
// MaintenancePlan stops a cluster's nodes on one host gracefully, one at a
// time, so the host can be patched or rebooted by its operator. The
// product never reboots or drains a host itself, and it does not change the
// Swarm node availability (that would also affect workloads it does not
// own). Before each stop every node of the cluster on another host must be
// Synced in the Primary component at the cluster size at that moment, so a
// stop never happens while the rest of the cluster is degraded; a graceful
// shutdown leaves the component cleanly, so the remaining nodes keep quorum.
// The plan is refused when the nodes outside the host are not a strict
// majority of the cluster (the cluster would then depend on one more node
// for its next failure). Traffic moves away from a stopped node through the
// proxy's health checks; while the host is in maintenance, replacement of
// its nodes is paused (ReplacementPolicy.PausedHosts).
//
// MaintenanceEndPlan starts the nodes again, one at a time: each start is
// preceded by an identity check of the node's volume (a reinstalled host
// with an empty volume would otherwise start a fresh node; lab E1/E2) and
// followed by a wait until the node is Synced at the grown cluster size
// (IST or SST from a donor).

// Plan kinds of host maintenance.
const (
	PlanHostMaintenance    PlanKind = "host-maintenance"
	PlanHostMaintenanceEnd PlanKind = "host-maintenance-end"
)

// Errors of maintenance planning.
var (
	// ErrNoNodesOnHost: the cluster has no node on the host.
	ErrNoNodesOnHost = errors.New("galera: cluster has no node on the host")
	// ErrMaintenanceUnsafe: the nodes outside the host are not a majority.
	ErrMaintenanceUnsafe = errors.New("galera: maintenance would leave no majority outside the host")
)

func splitByHost(nodes []Node, hostID string) (on, off []Node) {
	for _, n := range nodes {
		if n.HostID == hostID {
			on = append(on, n)
		} else {
			off = append(off, n)
		}
	}
	return on, off
}

// MaintenancePlan plans the graceful stop of the cluster's nodes on hostID.
func MaintenancePlan(c Cluster, nodes []Node, hostID string) (Plan, error) {
	on, off := splitByHost(nodes, hostID)
	if len(on) == 0 {
		return Plan{}, fmt.Errorf("%w: %s", ErrNoNodesOnHost, hostID)
	}
	if len(off)*2 <= len(nodes) {
		return Plan{}, fmt.Errorf("%w: %d of %d nodes would remain", ErrMaintenanceUnsafe, len(off), len(nodes))
	}
	p := Plan{Kind: PlanHostMaintenance, ClusterID: c.ClusterID, Cluster: c, Nodes: slices.Clone(nodes)}
	size := len(nodes)
	prev := ""
	for _, n := range on {
		spec, err := BuildServiceSpec(c, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		var pre []Precondition
		if prev != "" {
			pre = append(pre, Precondition{Kind: PreStepSucceeded, StepID: prev})
		}
		for _, o := range off {
			pre = append(pre, Precondition{Kind: PreNodeSynced, Slot: o.Slot, ClusterSize: size})
		}
		gate := Step{ID: "check-" + n.Slot, Kind: StepVerifyDonor, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, Preconditions: pre,
			Note: fmt.Sprintf("every node outside host %s is Synced at cluster size %d", hostID, size)}
		stop := Step{ID: "stop-" + n.Slot, Kind: StepStopAll, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, Spec: specPtr(spec), Preconditions: []Precondition{{Kind: PreStepSucceeded, StepID: gate.ID}},
			Note: "graceful shutdown (scale to 0, SIGTERM); the node leaves the component cleanly"}
		wait := Step{ID: "stopped-" + n.Slot, Kind: StepAwaitStopped, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, Spec: specPtr(spec), Preconditions: []Precondition{{Kind: PreStepSucceeded, StepID: stop.ID}}}
		p.Steps = append(p.Steps, gate, stop, wait)
		prev = wait.ID
		size--
	}
	p.Warnings = append(p.Warnings,
		"the manager does not reboot or drain the host; do that yourself once the operation succeeded",
		"clients connected directly to a stopped node lose their connections; proxies move traffic by their health checks")
	return p, nil
}

// MaintenanceEndPlan plans the start of the cluster's nodes on hostID
// after maintenance.
func MaintenanceEndPlan(c Cluster, nodes []Node, hostID string) (Plan, error) {
	on, off := splitByHost(nodes, hostID)
	if len(on) == 0 {
		return Plan{}, fmt.Errorf("%w: %s", ErrNoNodesOnHost, hostID)
	}
	p := Plan{Kind: PlanHostMaintenanceEnd, ClusterID: c.ClusterID, Cluster: c, Nodes: slices.Clone(nodes)}
	size := len(off)
	prev := ""
	for _, n := range on {
		spec, err := BuildServiceSpec(c, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		var pre []Precondition
		if prev != "" {
			pre = append(pre, Precondition{Kind: PreStepSucceeded, StepID: prev})
		}
		verify := Step{ID: "verify-" + n.Slot, Kind: StepVerifyIdentity, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, Preconditions: pre, Note: "the volume still carries this node's identity marker"}
		start := Step{ID: "start-" + n.Slot, Kind: StepStartNode, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, Spec: specPtr(spec), Preconditions: []Precondition{{Kind: PreStepSucceeded, StepID: verify.ID}}}
		size++
		synced := Step{ID: "synced-" + n.Slot, Kind: StepWaitSynced, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID,
			Volume: n.Volume, ClusterSize: size, Preconditions: []Precondition{{Kind: PreStepSucceeded, StepID: start.ID}},
			Note: "joins by IST or SST from a donor"}
		p.Steps = append(p.Steps, verify, start, synced)
		prev = synced.ID
	}
	return p, nil
}
