// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// EngineRuntimeInfo is what only the host's Docker daemon and the Swarm task
// list can tell. The shipping manager has no source yet; without one the
// engine guard fails closed.
type EngineRuntimeInfo struct {
	DockerInfoOK   bool
	RunningTasks   []string
	TasksKnown     bool
	LiveRestore    patch.Tristate
	TasksConverged patch.Tristate
	Version        string // the daemon's own answer, preferred over the inventory
}

// EngineRuntimeSource reads the daemon and task state of one host without
// changing it.
type EngineRuntimeSource interface {
	Runtime(ctx context.Context, host HostRecord) (EngineRuntimeInfo, error)
}

// PatchEngineProbe is the patch module's engine probe (owner decision
// 2026-10-30, ADR 0019/0023). Role, availability, state and the manager
// quorum math (shared with the engine update preview) come from the Swarm
// inventory; docker info, live-restore, tasks and reconvergence from the
// runtime source.
type PatchEngineProbe struct {
	Store   *Store
	Runtime EngineRuntimeSource
}

var _ patch.EngineProbe = (*PatchEngineProbe)(nil)

// Probe implements patch.EngineProbe.
func (p *PatchEngineProbe) Probe(ctx context.Context, hostID string) (patch.EngineState, error) {
	hosts, err := List[HostRecord](p.Store, CollHosts, Consistent())
	if err != nil {
		return patch.EngineState{}, err
	}
	var rec *HostRecord
	var swarmHosts []HostRecord
	for _, h := range hosts {
		if h.Value.NodeID == hostID {
			if rec != nil {
				return patch.EngineState{}, fmt.Errorf("host %s is recorded in several Swarms", hostID)
			}
			v := h.Value
			rec = &v
		}
	}
	if rec == nil {
		return patch.EngineState{}, fmt.Errorf("host %s is not in the Swarm inventory", hostID)
	}
	for _, h := range hosts {
		if h.Value.Swarm == rec.Swarm {
			swarmHosts = append(swarmHosts, h.Value)
		}
	}
	if p.Runtime == nil {
		return patch.EngineState{}, fmt.Errorf("no engine runtime source is configured")
	}
	info, err := p.Runtime.Runtime(ctx, *rec)
	if err != nil {
		return patch.EngineState{}, err
	}
	st := patch.EngineState{Version: rec.EngineVersion, DockerInfoOK: info.DockerInfoOK, SwarmRole: rec.Role, NodeState: rec.State,
		Availability: rec.Availability, RunningTasks: info.RunningTasks, TasksKnown: info.TasksKnown, LiveRestore: info.LiveRestore,
		TasksConverged: info.TasksConverged, QuorumSafe: patch.Unknown}
	if info.Version != "" {
		st.Version = info.Version
	}
	if rec.Role == "manager" {
		st.QuorumSafe = patch.No
		if managerQuorumSafe(swarmHosts, rec.NodeID) {
			st.QuorumSafe = patch.Yes
		}
	}
	return st, nil
}
