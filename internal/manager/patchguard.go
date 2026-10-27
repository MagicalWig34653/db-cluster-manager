// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// PatchGaleraGuard connects the OS patch service (ADR 0023) to the cluster
// planner's guided host maintenance (A08). It is both the membership
// directory and the coordinator of patch.GaleraGuard.
//
// A host is a Galera member when any cluster record has a node on it. Apply
// and reboot are permitted for a member only while the host is in guided
// maintenance, the coordination ID names one of the maintenance operations
// recorded on the host, and every cluster with a node on the host is covered
// by a succeeded host-maintenance operation of that list (its nodes are
// stopped gracefully after the rest of the cluster was checked to be Synced
// in the Primary component). Entering and leaving maintenance stay explicit
// operator or planner actions; this guard never starts them. Everything else
// is blocked (fail closed): unknown hosts records, a running, failed or
// unrelated operation, a cluster added after the maintenance began.
type PatchGaleraGuard struct {
	Store *Store
}

var (
	_ patch.GaleraDirectory = (*PatchGaleraGuard)(nil)
	_ patch.Coordinator     = (*PatchGaleraGuard)(nil)
)

// clustersOn returns the sorted IDs of the clusters with a node on the host
// (consistent read).
func (g *PatchGaleraGuard) clustersOn(hostID string) ([]string, error) {
	cs, err := List[ClusterRecord](g.Store, CollClusters, Consistent())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range cs {
		if slices.ContainsFunc(c.Value.Nodes, func(n galera.Node) bool { return n.HostID == hostID }) {
			ids = append(ids, c.Value.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Member implements patch.GaleraDirectory. With nodes of several clusters on
// the host the cluster IDs are joined with a comma.
func (g *PatchGaleraGuard) Member(_ context.Context, hostID string) (string, bool, error) {
	ids, err := g.clustersOn(hostID)
	if err != nil {
		return "", false, err
	}
	return strings.Join(ids, ","), len(ids) > 0, nil
}

// Permits implements patch.Coordinator.
func (g *PatchGaleraGuard) Permits(_ context.Context, coordination, clusterIDs, hostID string, _ patch.Action) (bool, error) {
	if coordination == "" {
		return false, nil
	}
	hosts, err := List[HostRecord](g.Store, CollHosts, Consistent())
	if err != nil {
		return false, err
	}
	var rec *HostRecord
	for _, h := range hosts {
		if h.Value.NodeID != hostID {
			continue
		}
		if rec != nil {
			return false, fmt.Errorf("host %s is recorded in several Swarms", hostID)
		}
		v := h.Value
		rec = &v
	}
	if rec == nil || rec.Maintenance == nil || !slices.Contains(rec.Maintenance.Operations, coordination) {
		return false, nil
	}
	covered := map[string]bool{}
	for _, id := range rec.Maintenance.Operations {
		op, ok, err := Get[OperationRecord](g.Store, CollOperations, id, Consistent())
		if err != nil {
			return false, err
		}
		if !ok || op.Value.Kind != "galera/"+string(galera.PlanHostMaintenance) ||
			op.Value.View == nil || op.Value.View.Status != runner.StatusSucceeded {
			return false, nil // any unfinished, failed or foreign operation: not ready
		}
		covered[op.Value.Cluster] = true
	}
	for _, c := range strings.Split(clusterIDs, ",") {
		if !covered[c] {
			return false, nil
		}
	}
	return true, nil
}
