// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Approval checks use the actual replicated store, health checks a recording
// fake. No TCP listener or infrastructure is needed for these HA tests.
type operationChecker struct{ svc *Service }

func (k operationChecker) Check(_ context.Context, _ string, _ runner.Step, c runner.Condition) (bool, string, error) {
	if c.Kind == runner.CondApproved {
		_, ok := StoreApprovals{Store: k.svc.Store()}.Approval(c.Target)
		return ok, "waiting for approval", nil
	}
	return true, "", nil
}

func seedOperationCluster(t *testing.T, s *Service, size int) *Operations {
	ops := seed(t, s, size+1)
	hosts := []galera.Host{}
	for i := 1; i <= size+1; i++ {
		hosts = append(hosts, galera.Host{ID: fmt.Sprintf("h%d", i), Ready: true, Active: true, Capacity: galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}})
	}
	req := createReq()
	p, err := galera.PlanCreate(galera.Topology{Cluster: galera.Cluster{InstallationID: "inst-1", ClusterID: "clu-ops", Name: "shop", Image: "mariadb:11.4.13@sha256:" + strings.Repeat("a", 64), Network: req.Network, RootPasswordSecret: req.RootPasswordSecret}, Size: size, Resources: req.Resources}, hosts, galera.PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	cr := ClusterRecord{ID: "clu-ops", Swarm: "sw1", Name: "shop", Spec: p.Cluster, Size: size, LastKnownSize: size, Nodes: p.Nodes, Resources: req.Resources, ServiceID: map[string]string{}, Health: map[string]NodeObservation{}}
	for _, n := range cr.Nodes {
		cr.ServiceID[n.Slot] = "svc-" + n.Slot
		cr.Health[n.Slot] = NodeObservation{ObservedAt: s.cfg.Now(), Health: galera.NodeHealth{State: galera.HealthHealthy, ClusterStatus: "Primary", LocalState: "Synced", ClusterSize: size, Ready: true, Connected: true, StateUUID: "history"}}
	}
	if _, err = Put(s.Store(), CollClusters, cr.ID, cr, nil); err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestGaleraOperationsApprovalAndFailover(t *testing.T) {
	for _, tc := range []struct {
		kind        galera.PlanKind
		size, want  int
		slot, image string
	}{
		{galera.PlanRollingRestart, 3, 3, "", ""},
		{galera.PlanMinorUpgrade, 3, 3, "", "mariadb:11.4.14@sha256:" + strings.Repeat("b", 64)},
		{galera.PlanScaleOut, 3, 4, "n4", ""},
		{galera.PlanScaleIn, 4, 3, "n4", ""},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			agents := newFakeAgents()
			entered := make(chan struct{}, 1)
			blocked := false
			agents.block = func(ctx context.Context, cmd agent.Command) error {
				agents.mu.Lock()
				b := blocked && cmd.Kind == agent.KindStopDB
				if b {
					blocked = false
				}
				agents.mu.Unlock()
				if b {
					entered <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			c := newClusterWith(t, 3, func(_ string, cfg *Config) { cfg.Agents = agents; cfg.AutoResume = true; cfg.Tune = galeraOpsRaft }, func(_ string, s *Service) { s.cfg.Checker = operationChecker{s} })
			old := c.leader()
			ops := seedOperationCluster(t, old, tc.size)
			req := galera.OperationRequest{Kind: tc.kind, Slot: tc.slot, Image: tc.image}
			if _, err := ops.PlanClusterOperation(nobody, "clu-ops", req); err == nil {
				t.Fatal("unauthorized plan")
			}
			pl, err := ops.PlanClusterOperation(adminSubject, "clu-ops", req)
			if err != nil {
				t.Fatal(err)
			}
			waitOpStatus(t, old, pl.ID, runner.StatusPaused)
			if len(agents.commands()) != 0 {
				t.Fatal("agent command before approval")
			}
			if _, err = ops.PlanClusterOperation(adminSubject, "clu-ops", req); !errors.Is(err, runner.ErrBusy) {
				t.Fatalf("busy: %v", err)
			}
			if _, err = ops.Approve(adminSubject, pl.ID, strings.Repeat("0", 64)); !errors.Is(err, ErrApprovalMismatch) {
				t.Fatalf("digest: %v", err)
			}
			agents.mu.Lock()
			blocked = true
			agents.mu.Unlock()
			if _, err = ops.Approve(adminSubject, pl.ID, pl.Approval.Digest); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(waitTimeout):
				t.Fatal("stop was never sent")
			}
			term := old.Current().Term
			c.close(old)
			next := c.leader(old.cfg.NodeID)
			done := waitOpStatus(t, next, pl.ID, runner.StatusSucceeded)
			if done.View.Term <= term {
				t.Fatal("not resumed in new term")
			}
			cr, _, err := Get[ClusterRecord](next.Store(), CollClusters, "clu-ops", Consistent())
			if err != nil {
				t.Fatal(err)
			}
			if len(cr.Value.Nodes) != tc.want || cr.Value.Size != tc.want || cr.Value.LastKnownSize != tc.want {
				t.Fatalf("result not committed: %+v", cr.Value)
			}
			if tc.image != "" && cr.Value.Spec.Image != tc.image {
				t.Fatal("upgrade image not committed")
			}
			if tc.kind == galera.PlanScaleIn {
				if _, ok := cr.Value.ServiceID[tc.slot]; ok {
					t.Fatal("removed service retained in inventory")
				}
			}
			if tc.kind == galera.PlanScaleOut && cr.Value.ServiceID[tc.slot] == "" {
				t.Fatal("added service ID not recorded")
			}
		})
	}
}

// The last step may already be committed while the manager's desired-state
// transaction is still missing. TakeOver excludes a fully succeeded journal.
func TestGaleraOperationsReconcileCompletedJournal(t *testing.T) {
	agents := newFakeAgents()
	c := newClusterWith(t, 3, func(_ string, cfg *Config) { cfg.Agents = agents; cfg.Tune = galeraOpsRaft }, func(_ string, s *Service) { s.cfg.Checker = operationChecker{s} })
	old := c.leader()
	ops := seedOperationCluster(t, old, 3)
	target := "mariadb:11.4.14@sha256:" + strings.Repeat("b", 64)
	p, err := ops.PlanClusterOperation(adminSubject, "clu-ops", galera.OperationRequest{Kind: galera.PlanMinorUpgrade, Image: target})
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, old, p.ID, runner.StatusPaused)
	if _, err = ops.Approve(adminSubject, p.ID, p.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, old, p.ID, runner.StatusSucceeded)
	// Stop the leader's view writer, then write the pre-transaction snapshot
	// through a follower after election, before explicitly reconciling it.
	c.close(old)
	next := c.leader(old.cfg.NodeID)
	_, _, err = Update(next.Store(), CollOperations, p.ID, func(r OperationRecord, _ bool) (OperationRecord, error) {
		r.EndedAt = time.Time{}
		r.View = &runner.View{ID: p.ID, Status: runner.StatusPaused}
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Update(next.Store(), CollClusters, "clu-ops", func(r ClusterRecord, _ bool) (ClusterRecord, error) {
		r.Spec.Image = "mariadb:11.4.13@sha256:" + strings.Repeat("a", 64)
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next.reconcileGaleraSuccesses()
	r := opRecord(t, next, p.ID)
	if r.EndedAt.IsZero() || r.View.Status != runner.StatusSucceeded {
		t.Fatal("completed journal was left pending")
	}
	cl, _, err := Get[ClusterRecord](next.Store(), CollClusters, "clu-ops", Consistent())
	if err != nil || cl.Value.Spec.Image != target {
		t.Fatalf("desired image not recovered: %s %v", cl.Value.Spec.Image, err)
	}
}

// Race instrumentation and concurrent package tests can exceed the shared
// 50 ms prototype election timeout; keep these HA tests above that jitter.
func galeraOpsRaft(c *metastore.Config) {
	fastRaft(c)
	c.HeartbeatTimeout = 300 * time.Millisecond
	c.ElectionTimeout = 300 * time.Millisecond
	c.LeaderLeaseTimeout = 150 * time.Millisecond
}
