// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// RoutingChecker is the runner.Checker of the manager. It routes each
// condition to the component that can evaluate it and fails closed for
// everything it cannot evaluate:
//
//   - CondApproved: the replicated approval store (digest-bound).
//   - dbbackup conditions: a dbbackup.Checker built per call, bound to the
//     operation's cluster; without a backup backend they are unmet.
//   - everything else (node-synced, quorum, ...): Cluster, typically the
//     live galeraprobe.Checker.
type RoutingChecker struct {
	Store   *Store
	Cluster runner.Checker
	// Backup evaluates backend conditions; its Nodes and Approvals are
	// replaced per call. Nil leaves them unmet.
	Backup *dbbackup.Checker
	// Ops resolves an operation's cluster and hosts for CondSourceEligible
	// (only Svc is used).
	Ops *Operations
}

var _ runner.Checker = (*RoutingChecker)(nil)

// Check implements runner.Checker.
func (c *RoutingChecker) Check(ctx context.Context, opID string, step runner.Step, cond runner.Condition) (bool, string, error) {
	switch cond.Kind {
	case dbbackup.CondApproved:
		a, ok := StoreApprovals{Store: c.Store}.Approval(cond.Target)
		if !ok || a.By == "" {
			return false, "approval " + cond.Target + " not recorded for this operation definition", nil
		}
		return true, "", nil
	case dbbackup.CondSourceEligible, dbbackup.CondBackupComplete, dbbackup.CondRestoreComplete, dbbackup.CondRestoreVerified:
		if c.Backup == nil {
			return false, "no backup backend configured", nil
		}
		bc := *c.Backup
		bc.Approvals = StoreApprovals{Store: c.Store}
		bc.Nodes = func(context.Context) ([]dbbackup.NodeState, error) { return c.opNodes(opID) }
		if bc.SessionReport == nil {
			bc.SessionReport = func(id string) (dbbackup.VerifyReport, bool) {
				r, ok, err := Get[dbbackup.VerifyReport](c.Store, CollVerifications, id)
				return r.Value, err == nil && ok
			}
		}
		return bc.Check(ctx, opID, step, cond)
	}
	if c.Cluster == nil {
		return false, "no checker for " + cond.Kind, nil
	}
	return c.Cluster.Check(ctx, opID, step, cond)
}

// opNodes is the source-selection view of the operation's cluster from
// fresh observations (the live node-synced condition is checked by the
// cluster checker in the same gate).
func (c *RoutingChecker) opNodes(opID string) ([]dbbackup.NodeState, error) {
	if c.Ops == nil {
		return nil, fmt.Errorf("manager: no operations for source checks")
	}
	rec, ok, err := Get[OperationRecord](c.Store, CollOperations, opID)
	if err != nil {
		return nil, err
	}
	if !ok || rec.Value.Cluster == "" {
		return nil, fmt.Errorf("%w: operation %s has no cluster", ErrNotFound, opID)
	}
	cl, ok, err := Get[ClusterRecord](c.Store, CollClusters, rec.Value.Cluster)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: cluster %s", ErrNotFound, rec.Value.Cluster)
	}
	b := &Backups{Ops: c.Ops}
	return b.nodeStates(cl.Value)
}
