// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// TestAbortRefusedWhileOperationLeftNodeStopped: a rolling restart that
// stopped a database service (scaled to 0) and failed before starting it
// again must not be abortable, otherwise the node stays at 0 replicas with no
// operation left to start it (lab E13). The operator resumes the operation
// or starts the node by hand first (not exercised here: the fake caches the
// refused answer per step).
func TestAbortRefusedWhileOperationLeftNodeStopped(t *testing.T) {
	agents := newFakeAgents()
	agents.refuse = func(cmd agent.Command) bool { return cmd.Kind == agent.KindStartService }
	c := newClusterWith(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents }, func(_ string, s *Service) { s.cfg.Checker = operationChecker{s} })
	l := c.leader()
	ops := seedOperationCluster(t, l, 3)
	ops.MaxRetries = 0
	pl, err := ops.PlanClusterOperation(adminSubject, "clu-ops", galera.OperationRequest{Kind: galera.PlanRollingRestart})
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, pl.ID, runner.StatusPaused)
	if _, err = ops.Approve(adminSubject, pl.ID, pl.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, pl.ID, runner.StatusFailed)
	if err := ops.Abort(adminSubject, pl.ID); !errors.Is(err, ErrAbortLeavesNodeStopped) {
		t.Fatalf("abort with a stopped node: %v", err)
	}
	if got := MapCommandError(ErrAbortLeavesNodeStopped); got == nil || got.Error() == ErrAbortLeavesNodeStopped.Error() {
		t.Fatalf("not mapped to an API sentinel: %v", got)
	}
}

// TestAbortAllowedBeforeAnyNodeWasStopped: the refusal applies only after a
// stop step ran.
func TestAbortAllowedBeforeAnyNodeWasStopped(t *testing.T) {
	agents := newFakeAgents()
	agents.refuse = func(cmd agent.Command) bool { return cmd.Kind == agent.KindStopDB }
	c := newClusterWith(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents }, func(_ string, s *Service) { s.cfg.Checker = operationChecker{s} })
	l := c.leader()
	ops := seedOperationCluster(t, l, 3)
	ops.MaxRetries = 0
	pl, err := ops.PlanClusterOperation(adminSubject, "clu-ops", galera.OperationRequest{Kind: galera.PlanRollingRestart})
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, pl.ID, runner.StatusPaused)
	if err := ops.Abort(adminSubject, pl.ID); err != nil {
		t.Fatalf("abort before any stop: %v", err)
	}
	waitOpStatus(t, l, pl.ID, runner.StatusAborted)
}
