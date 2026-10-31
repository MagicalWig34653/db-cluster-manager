// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"

	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// RoutingChecker is the runner.Checker of the manager. It routes each
// condition to the component that can evaluate it and fails closed for
// everything it cannot evaluate:
//
//   - CondApproved: the replicated approval store (digest-bound).
//   - dbbackup conditions: a dbbackup.Checker built per call, bound to the
//     operation's cluster; without a backup backend they are unmet.
//   - CondRestoreVerified: the stored verification report of the session,
//     or a probe of the inspection instance that records one (verify.go).
//   - everything else (node-synced, quorum, ...): Cluster, typically the
//     live galeraprobe.Checker.
type RoutingChecker struct {
	Store   *Store
	Cluster runner.Checker
	// Backup evaluates backend conditions; its Nodes and Approvals are
	// replaced per call. Nil leaves them unmet.
	Backup *dbbackup.Checker
	// Ops resolves an operation's cluster and hosts for CondSourceEligible
	// (Svc), and observes services for runner.CondServiceHealthy (Svc and
	// Swarms; without Swarms the condition is unmet).
	Ops *Operations
	// Registry judges service health; nil is service.DefaultRegistry.
	Registry *service.Registry
	// JobIdleGrace bounds how long a backup or restore job service may run
	// no task before a result gate fails the operation (default
	// DefaultJobIdleGrace). Now defaults to time.Now.
	JobIdleGrace time.Duration
	Now          func() time.Time

	jobs jobWatch
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
	case dbbackup.CondRestoreVerified:
		return c.restoreVerified(ctx, cond.Target)
	case runner.CondServiceHealthy:
		return c.serviceHealthy(ctx, cond.Target)
	case dbbackup.CondSourceEligible, dbbackup.CondBackupComplete, dbbackup.CondRestoreComplete:
		if c.Backup == nil {
			return false, "no backup backend configured", nil
		}
		bc := *c.Backup
		bc.Approvals = StoreApprovals{Store: c.Store}
		bc.Nodes = func(context.Context) ([]dbbackup.NodeState, error) { return c.opNodes(opID) }
		if cond.Kind == dbbackup.CondSourceEligible && (bc.Policy.Job.MilliCPU <= 0 || bc.Policy.Job.MemoryBytes <= 0) {
			// The source re-check needs the job's demand like the planning
			// did (lab E4: without it every backup paused at check-source).
			job, err := c.opJobDemand(opID)
			if err != nil {
				return false, "", err
			}
			bc.Policy.Job = job
		}
		ok, why, err := bc.Check(ctx, opID, step, cond)
		if ok || err != nil || (cond.Kind != dbbackup.CondBackupComplete && cond.Kind != dbbackup.CondRestoreComplete) {
			return ok, why, err
		}
		if dead, isDead := c.jobDead(ctx, opID, cond); isDead {
			// The job may have written its result between the two reads.
			ok, why2, err := bc.Check(ctx, opID, step, cond)
			if ok || err != nil {
				return ok, why2, err
			}
			return false, dead + " (" + why2 + ")", runner.ErrConditionFatal
		}
		return false, why, nil
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

// opJobDemand returns the resource demand of the operation's backup job
// (the limits of its first run-backup step), in the placement unit.
func (c *RoutingChecker) opJobDemand(opID string) (galera.ResourceAmount, error) {
	rec, ok, err := Get[OperationRecord](c.Store, CollOperations, opID)
	if err != nil {
		return galera.ResourceAmount{}, err
	}
	if !ok {
		return galera.ResourceAmount{}, fmt.Errorf("%w: operation %s", ErrNotFound, opID)
	}
	for _, st := range rec.Value.Definition.Steps {
		if st.Command != agent.KindRunBackup {
			continue
		}
		var a agent.RunBackupArgs
		if err := json.Unmarshal(st.Args, &a); err != nil {
			return galera.ResourceAmount{}, fmt.Errorf("operation %s step %s: %w", opID, st.ID, err)
		}
		return galera.ResourceAmount{MilliCPU: a.Resources.NanoCPUs / 1e6, MemoryBytes: a.Resources.MemoryBytes}, nil
	}
	return galera.ResourceAmount{}, fmt.Errorf("%w: operation %s has no backup job", ErrNotFound, opID)
}
