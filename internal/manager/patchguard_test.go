// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// TestPatchGaleraGuardFollowsGuidedMaintenance: a Galera member host may only
// be patched or rebooted while the planner's guided host maintenance has
// stopped its nodes (operations succeeded) and the request names exactly
// that coordination.
func TestPatchGaleraGuardFollowsGuidedMaintenance(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	l := c.leader()
	ops, _ := replaceFixture(t, l, 2)
	pg := &PatchGaleraGuard{Store: l.Store()}
	guard := patch.GaleraGuard{Directory: pg, Coordinator: pg}
	ctx := context.Background()

	// A host without cluster nodes is not a member.
	if err := guard.Check(ctx, "h4", patch.ActionApply, ""); err != nil {
		t.Fatal("non-member blocked:", err)
	}
	// A member without coordination, or with an invented one, is blocked.
	for _, coord := range []string{"", "op-invented"} {
		if err := guard.Check(ctx, "h3", patch.ActionApply, coord); !errors.Is(err, patch.ErrGaleraBlocked) {
			t.Fatalf("coordination %q: %v", coord, err)
		}
	}
	started, err := ops.EnterMaintenance(adminSubject, "sw1", "h3", "kernel update")
	if err != nil {
		t.Fatal(err)
	}
	opID := started.Operations[0].ID
	waitOpStatus(t, l, opID, runner.StatusSucceeded)
	for _, a := range []patch.Action{patch.ActionApply, patch.ActionReboot} {
		if err := guard.Check(ctx, "h3", a, opID); err != nil {
			t.Fatalf("%s in ready maintenance: %v", a, err)
		}
	}
	// Another host's maintenance or a wrong coordination does not count.
	if err := guard.Check(ctx, "h2", patch.ActionApply, opID); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("coordination of another host accepted:", err)
	}
	if err := guard.Check(ctx, "h3", patch.ActionApply, "op-other"); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("wrong coordination accepted:", err)
	}
	// Leaving maintenance ends the permission.
	left, err := ops.LeaveMaintenance(adminSubject, "sw1", "h3")
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, left.Operations[0].ID, runner.StatusSucceeded)
	if err := guard.Check(ctx, "h3", patch.ActionApply, opID); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("permission survived the end of maintenance:", err)
	}
	// The end operation itself is never a coordination.
	if err := guard.Check(ctx, "h3", patch.ActionReboot, left.Operations[0].ID); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("end operation accepted:", err)
	}
}

// A maintenance whose operation failed or is unfinished, or that does not
// cover every cluster of the host, permits nothing.
func TestPatchGaleraGuardFailsClosed(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	l := c.leader()
	_, cl := replaceFixture(t, l, 2)
	pg := &PatchGaleraGuard{Store: l.Store()}
	guard := patch.GaleraGuard{Directory: pg, Coordinator: pg}
	ctx := context.Background()
	kind := "galera/" + string(galera.PlanHostMaintenance)
	mark := func(ops ...string) {
		t.Helper()
		h, _, _ := Get[HostRecord](l.Store(), CollHosts, HostKey("sw1", "h3"))
		hv := h.Value
		hv.Maintenance = &HostMaintenanceRecord{Operations: ops}
		if _, err := Put(l.Store(), CollHosts, HostKey("sw1", "h3"), hv, nil); err != nil {
			t.Fatal(err)
		}
	}
	put := func(id, cluster string, st runner.Status) {
		t.Helper()
		if _, err := Put(l.Store(), CollOperations, id, OperationRecord{ID: id, Kind: kind, Cluster: cluster, View: &runner.View{Status: st}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put("op-run", "clu-1", runner.StatusRunning)
	mark("op-run")
	if err := guard.Check(ctx, "h3", patch.ActionApply, "op-run"); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("running operation permits:", err)
	}
	put("op-ok", "clu-1", runner.StatusSucceeded)
	mark("op-ok")
	if err := guard.Check(ctx, "h3", patch.ActionApply, "op-ok"); err != nil {
		t.Fatal(err)
	}
	// A second cluster with a node on h3 appeared after the maintenance began.
	cl2 := cl
	cl2.ID, cl2.Name = "clu-2", "other"
	cl2.Spec.ClusterID = "clu-2"
	if _, err := Put(l.Store(), CollClusters, cl2.ID, cl2, nil); err != nil {
		t.Fatal(err)
	}
	if err := guard.Check(ctx, "h3", patch.ActionApply, "op-ok"); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("uncovered cluster permitted:", err)
	}
	put("op-ok2", "clu-2", runner.StatusSucceeded)
	mark("op-ok", "op-ok2")
	if err := guard.Check(ctx, "h3", patch.ActionReboot, "op-ok2"); err != nil {
		t.Fatal("both clusters covered:", err)
	}
	// A wrong-kind operation in the list does not count as a stop.
	if _, err := Put(l.Store(), CollOperations, "op-ok2", OperationRecord{ID: "op-ok2", Kind: "galera/rolling-restart", Cluster: "clu-2", View: &runner.View{Status: runner.StatusSucceeded}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := guard.Check(ctx, "h3", patch.ActionReboot, "op-ok2"); !errors.Is(err, patch.ErrGaleraBlocked) {
		t.Fatal("wrong operation kind permitted:", err)
	}
}
