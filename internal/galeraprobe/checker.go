// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// HealthProber reads the Galera health of one node. *Prober implements it.
type HealthProber interface {
	Probe(ctx context.Context, addr string) (galera.NodeHealth, error)
}

// Topology maps plan references to database addresses (host:port).
type Topology interface {
	// SlotAddress returns the address of a node slot of the operation's cluster.
	SlotAddress(ctx context.Context, opID, slot string) (string, error)
	// ClusterAddresses returns the addresses of all nodes of the operation's
	// cluster, used to count Primary-component members.
	ClusterAddresses(ctx context.Context, opID string) ([]string, error)
}

// Checker evaluates the live-state preconditions of Galera plans:
//
//   - node-synced (wait-synced and verify-donor steps): the slot's node is
//     healthy, i.e. Primary, connected, ready and Synced (a Donor/Desynced,
//     Joined, flow-control-throttled or unknown node does not qualify, so a
//     donor must be fully Synced), and wsrep_cluster_size equals the
//     condition's ClusterSize.
//   - state-recovered (zero-UUID recovery check): exactly ClusterSize nodes
//     answer, each is Synced and healthy in a Primary component of that
//     size, and all report one common non-zero wsrep_cluster_state_uuid.
//   - quorum: at least ClusterSize nodes are members of the Primary
//     component and they agree on wsrep_cluster_state_uuid (differing UUIDs
//     indicate split clusters and fail).
//
// Anything unreachable, unknown or ambiguous is "not met" (fail closed). The
// approved and no-node-running conditions are not live database state; they
// are delegated to Fallback, and unmet when it is nil.
type Checker struct {
	Prober   HealthProber
	Topology Topology
	// Fallback evaluates all other condition kinds; optional.
	Fallback runner.Checker
}

var _ runner.Checker = (*Checker)(nil)

// Check implements runner.Checker.
func (k *Checker) Check(ctx context.Context, opID string, step runner.Step, c runner.Condition) (bool, string, error) {
	if k.Prober == nil || k.Topology == nil {
		return false, "", errors.New("galeraprobe: Checker needs Prober and Topology")
	}
	switch c.Kind {
	case runner.CondNodeSynced:
		return k.nodeSynced(ctx, opID, c)
	case runner.CondQuorum:
		return k.quorum(ctx, opID, c)
	case runner.CondStateRecovered:
		return k.stateRecovered(ctx, opID, c)
	}
	if k.Fallback != nil {
		return k.Fallback.Check(ctx, opID, step, c)
	}
	return false, fmt.Sprintf("condition %q is not evaluated by the Galera checker", c.Kind), nil
}

func (k *Checker) nodeSynced(ctx context.Context, opID string, c runner.Condition) (bool, string, error) {
	if c.Target == "" || c.ClusterSize < 1 {
		return false, "node-synced needs a slot and a cluster size", nil
	}
	addr, err := k.Topology.SlotAddress(ctx, opID, c.Target)
	if err != nil {
		return false, "", fmt.Errorf("resolving slot %s: %w", c.Target, err)
	}
	h, err := k.Prober.Probe(ctx, addr)
	if err != nil {
		return false, "node unreachable: " + err.Error(), nil
	}
	if !h.Healthy() || !h.DonorCapable() {
		return false, fmt.Sprintf("node is %s (%s)", h.State, strings.Join(h.Reasons, "; ")), nil
	}
	if h.ClusterSize != c.ClusterSize {
		return false, fmt.Sprintf("wsrep_cluster_size is %d, want %d", h.ClusterSize, c.ClusterSize), nil
	}
	return true, "", nil
}

func (k *Checker) quorum(ctx context.Context, opID string, c runner.Condition) (bool, string, error) {
	if c.ClusterSize < 1 {
		return false, "quorum needs a positive cluster size", nil
	}
	addrs, err := k.Topology.ClusterAddresses(ctx, opID)
	if err != nil {
		return false, "", fmt.Errorf("resolving cluster nodes: %w", err)
	}
	healths := make([]galera.NodeHealth, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := k.Prober.Probe(ctx, a)
			if err == nil {
				healths[i] = h
			}
		}()
	}
	wg.Wait()
	in := 0
	uuids := map[string]bool{}
	for _, h := range healths {
		if h.InPrimary() && h.StateUUID != "" {
			in++
			uuids[h.StateUUID] = true
		}
	}
	if len(uuids) > 1 {
		return false, "Primary members disagree on wsrep_cluster_state_uuid", nil
	}
	if in < c.ClusterSize {
		return false, fmt.Sprintf("%d of %d nodes in the Primary component, need %d", in, len(addrs), c.ClusterSize), nil
	}
	return true, "", nil
}

func (k *Checker) stateRecovered(ctx context.Context, opID string, c runner.Condition) (bool, string, error) {
	if c.ClusterSize < 1 {
		return false, "state-recovered needs a positive cluster size", nil
	}
	addrs, err := k.Topology.ClusterAddresses(ctx, opID)
	if err != nil {
		return false, "", fmt.Errorf("resolving cluster nodes: %w", err)
	}
	healths := make([]galera.NodeHealth, len(addrs))
	errs := make([]error, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			healths[i], errs[i] = k.Prober.Probe(ctx, a)
		}()
	}
	wg.Wait()
	synced := 0
	uuids := map[string]bool{}
	for i, h := range healths {
		switch {
		case errs[i] != nil:
			return false, fmt.Sprintf("node %s unreachable: %v", addrs[i], errs[i]), nil
		case !h.DonorCapable():
			return false, fmt.Sprintf("node %s is not Synced (%s, %s)", addrs[i], h.State, strings.Join(h.Reasons, "; ")), nil
		case h.ClusterSize != c.ClusterSize:
			return false, fmt.Sprintf("node %s: wsrep_cluster_size is %d, want %d", addrs[i], h.ClusterSize, c.ClusterSize), nil
		case h.StateUUID == "" || h.StateUUID == galera.ZeroUUID:
			return false, fmt.Sprintf("node %s still reports the zero cluster UUID", addrs[i]), nil
		}
		synced++
		uuids[h.StateUUID] = true
	}
	if len(uuids) > 1 {
		return false, "nodes disagree on wsrep_cluster_state_uuid", nil
	}
	if synced < c.ClusterSize {
		return false, fmt.Sprintf("%d of %d nodes Synced", synced, c.ClusterSize), nil
	}
	return true, "", nil
}
