// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"errors"
	"fmt"
	"sort"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// ErrNoSource means no node may serve as backup source right now.
var ErrNoSource = errors.New("dbbackup: no eligible backup source node")

// NodeState is the observed state of one cluster node, as the manager sees
// it at planning time and again when a gate is evaluated.
type NodeState struct {
	Slot     string            `json:"slot"`
	NodeUUID string            `json:"node_uuid"`
	HostID   string            `json:"host_id"`
	Volume   string            `json:"volume"`
	Health   galera.NodeHealth `json:"health"`
	// Writer marks the node the proxy currently routes writes to.
	Writer bool `json:"writer,omitempty"`
	// HostFree is the unallocated capacity of the node's host.
	HostFree galera.ResourceAmount `json:"host_free"`
}

// SourcePolicy tunes source selection.
type SourcePolicy struct {
	// ClusterSize is the expected cluster size; every candidate must report it.
	ClusterSize int
	// AllowWriter permits the writer as a last resort. Off by default:
	// mariabackup's BACKUP STAGE BLOCK_COMMIT briefly blocks commits on the
	// source node, which on the writer stalls clients.
	AllowWriter bool
	// MaxFlowControlPaused rejects nodes (and the whole cluster) above this
	// fraction of time paused by flow control. Default 0.1.
	MaxFlowControlPaused float64
	// Job is the job's CPU and memory demand that must fit on the host.
	Job galera.ResourceAmount
}

// Rejection explains why a node was not chosen.
type Rejection struct {
	Slot   string `json:"slot"`
	Reason string `json:"reason"`
}

func (p SourcePolicy) maxFC() float64 {
	if p.MaxFlowControlPaused <= 0 {
		return 0.1
	}
	return p.MaxFlowControlPaused
}

// eligible applies the per-node rules. It does not consider the writer rule.
func (p SourcePolicy) eligible(n NodeState) error {
	h := n.Health
	switch {
	case h.LocalState == "Donor/Desynced":
		// A donor serving SST (or a node desynced by an operator) is never
		// used: the backup would compete with the state transfer, and a
		// desynced node's data lags the cluster.
		return fmt.Errorf("node is %s (serving a state transfer or desynced)", h.LocalState)
	case !h.DonorCapable():
		return fmt.Errorf("node is not Synced and healthy (state %q, local state %q)", h.State, h.LocalState)
	case p.ClusterSize > 0 && h.ClusterSize != p.ClusterSize:
		return fmt.Errorf("node sees cluster size %d, expected %d", h.ClusterSize, p.ClusterSize)
	case h.FlowControlPaused > p.maxFC():
		return fmt.Errorf("flow control paused %.2f above %.2f", h.FlowControlPaused, p.maxFC())
	case n.HostID == "" || n.Volume == "" || n.NodeUUID == "":
		return errors.New("node has no host, volume or identity")
	case n.HostFree.MilliCPU < p.Job.MilliCPU || n.HostFree.MemoryBytes < p.Job.MemoryBytes:
		return fmt.Errorf("host %s lacks capacity for the backup job (free %d mCPU/%d B, need %d/%d)",
			n.HostID, n.HostFree.MilliCPU, n.HostFree.MemoryBytes, p.Job.MilliCPU, p.Job.MemoryBytes)
	}
	return nil
}

// SelectSource picks the backup source: a Synced, healthy, non-writer node
// with the least flow control pause (ties by slot). It refuses when:
//
//   - any node is Joiner/Donor (a state transfer is running): adding a
//     backup's I/O and commit blocking to an SST invites flow control and
//     slows recovery;
//   - fewer than two nodes are Synced: the backup would load the only node
//     that serves the cluster;
//   - only the writer is eligible and AllowWriter is off.
func SelectSource(nodes []NodeState, p SourcePolicy) (NodeState, []Rejection, error) {
	if p.Job.MilliCPU <= 0 || p.Job.MemoryBytes <= 0 {
		return NodeState{}, nil, fmt.Errorf("%w: job CPU and memory demand required", ErrInvalidRequest)
	}
	var rej []Rejection
	synced := 0
	for _, n := range nodes {
		switch n.Health.LocalState {
		case "Joiner", "Joined":
			return NodeState{}, nil, fmt.Errorf("%w: node %s is %s; wait for the state transfer to finish", ErrNoSource, n.Slot, n.Health.LocalState)
		}
		if n.Health.DonorCapable() {
			synced++
		}
	}
	if synced < 2 {
		return NodeState{}, nil, fmt.Errorf("%w: only %d Synced node(s); a backup must not load the last serving node", ErrNoSource, synced)
	}
	var cands []NodeState
	for _, n := range nodes {
		if err := p.eligible(n); err != nil {
			rej = append(rej, Rejection{Slot: n.Slot, Reason: err.Error()})
			continue
		}
		cands = append(cands, n)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Writer != b.Writer {
			return !a.Writer
		}
		if a.Health.FlowControlPaused != b.Health.FlowControlPaused {
			return a.Health.FlowControlPaused < b.Health.FlowControlPaused
		}
		return a.Slot < b.Slot
	})
	for _, c := range cands {
		if c.Writer && !p.AllowWriter {
			rej = append(rej, Rejection{Slot: c.Slot, Reason: "node is the writer (AllowWriter is off)"})
			continue
		}
		return c, rej, nil
	}
	return NodeState{}, rej, fmt.Errorf("%w: %d node(s) rejected", ErrNoSource, len(rej))
}

// CheckSource re-evaluates the rules for one chosen slot (used by the gate
// right before the backup job starts).
func CheckSource(nodes []NodeState, slot string, p SourcePolicy) error {
	chosen, _, err := SelectSource(nodes, p)
	if err != nil {
		// The chosen slot may still be fine if only the ranking changed;
		// SelectSource errors are cluster-level refusals.
		return err
	}
	if chosen.Slot == slot {
		return nil
	}
	for _, n := range nodes {
		if n.Slot != slot {
			continue
		}
		if err := p.eligible(n); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrNoSource, slot, err)
		}
		if n.Writer && !p.AllowWriter {
			return fmt.Errorf("%w: %s became the writer", ErrNoSource, slot)
		}
		return nil
	}
	return fmt.Errorf("%w: slot %s not observed", ErrNoSource, slot)
}
