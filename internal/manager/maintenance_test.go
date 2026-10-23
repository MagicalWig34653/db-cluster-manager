// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func sentKinds(a *fakeAgents, opID string) []agent.Kind {
	var out []agent.Kind
	for _, c := range a.commands() {
		if c.Cmd.OpID == opID {
			out = append(out, c.Cmd.Kind)
		}
	}
	return out
}

// TestHostMaintenanceStopsAndRestartsNodes: the preview changes nothing;
// entering maintenance pauses replacement on the host and stops its node
// gracefully after the sync check; the host reports ready; leaving
// maintenance verifies the identity, starts the node and waits for Synced.
func TestHostMaintenanceStopsAndRestartsNodes(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	l := c.leader()
	ops, _ := replaceFixture(t, l, 2)
	ctl := &Control{Ops: ops, Swarms: &Swarms{}}

	pv, err := ops.MaintenancePreview(adminSubject, "sw1", "h3", false)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Safe || len(pv.Clusters) != 1 || len(pv.Clusters[0].Steps) != 3 || pv.Clusters[0].Steps[1].Kind != string(galera.StepStopAll) ||
		len(pv.Clusters[0].Steps[0].Conditions) != 2 {
		t.Fatalf("preview %+v", pv)
	}
	if len(agents.commands()) != 0 {
		t.Fatal("preview sent commands")
	}
	if _, err := ops.EnterMaintenance(nobody, "sw1", "h3", ""); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized: %v", err)
	}

	started, err := ops.EnterMaintenance(adminSubject, "sw1", "h3", "kernel update")
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Operations) != 1 || started.Operations[0].Kind != "galera/"+string(galera.PlanHostMaintenance) {
		t.Fatalf("started %+v", started)
	}
	opID := started.Operations[0].ID
	waitOpStatus(t, l, opID, runner.StatusSucceeded)
	if got := sentKinds(agents, opID); !slices.Equal(got, []agent.Kind{agent.KindStopDB, agent.KindAwaitStopped}) {
		t.Fatalf("commands %v", got)
	}
	hosts, _ := ctl.Hosts(context.Background())
	var h3 *api.Host
	for i := range hosts {
		if hosts[i].NodeID == "h3" {
			h3 = &hosts[i]
		}
	}
	if h3 == nil || h3.Maintenance == nil || !h3.Maintenance.Ready || h3.Maintenance.Reason != "kernel update" || h3.Maintenance.Operations[0] != opID {
		t.Fatalf("host view %+v", h3)
	}
	// Replacement of the host's node is paused.
	if _, err := ops.ReplaceNode(adminSubject, "clu-1", "n3"); !errors.Is(err, galera.ErrReplacementPaused) {
		t.Fatalf("replacement during maintenance: %v", err)
	}
	if _, err := ops.EnterMaintenance(adminSubject, "sw1", "h3", ""); !errors.Is(err, ErrInMaintenance) {
		t.Fatalf("second enter: %v", err)
	}

	left, err := ops.LeaveMaintenance(adminSubject, "sw1", "h3")
	if err != nil {
		t.Fatal(err)
	}
	endID := left.Operations[0].ID
	waitOpStatus(t, l, endID, runner.StatusSucceeded)
	if got := sentKinds(agents, endID); !slices.Equal(got, []agent.Kind{agent.KindVerifyIdentity, agent.KindStartService}) {
		t.Fatalf("commands %v", got)
	}
	h, _, _ := Get[HostRecord](l.Store(), CollHosts, HostKey("sw1", "h3"))
	if h.Value.Maintenance != nil {
		t.Fatal("maintenance mark not cleared")
	}
	if _, err := ops.LeaveMaintenance(adminSubject, "sw1", "h3"); !errors.Is(err, ErrNotInMaintenance) {
		t.Fatalf("second leave: %v", err)
	}
}

// TestHostMaintenanceRefusesBusyOrUnsafeClusters: nothing is marked or
// started when an affected cluster has an unfinished operation or would
// keep no majority outside the host.
func TestHostMaintenanceRefusesBusyOrUnsafeClusters(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	l := c.leader()
	ops, cl := replaceFixture(t, l, 2)
	if _, err := Put(l.Store(), CollOperations, "op-x", OperationRecord{ID: "op-x", Cluster: "clu-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.EnterMaintenance(adminSubject, "sw1", "h2", ""); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if _, err := Put(l.Store(), CollOperations, "op-x", OperationRecord{ID: "op-x", Cluster: "clu-1", EndedAt: l.cfg.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	// Two nodes on h2: one would remain.
	cl.Nodes[2].HostID = "h2"
	if _, err := Put(l.Store(), CollClusters, cl.ID, cl, nil); err != nil {
		t.Fatal(err)
	}
	pv, err := ops.MaintenancePreview(adminSubject, "sw1", "h2", false)
	if err != nil || pv.Safe || pv.Clusters[0].Refused == "" {
		t.Fatalf("preview %+v %v", pv, err)
	}
	if _, err := ops.EnterMaintenance(adminSubject, "sw1", "h2", ""); !errors.Is(err, galera.ErrMaintenanceUnsafe) {
		t.Fatalf("unsafe: %v", err)
	}
	h, _, _ := Get[HostRecord](l.Store(), CollHosts, HostKey("sw1", "h2"))
	if h.Value.Maintenance != nil || len(agents.commands()) != 0 {
		t.Fatal("a refused maintenance changed something")
	}
	// A host without cluster nodes enters maintenance at once and is ready.
	st, err := ops.EnterMaintenance(adminSubject, "sw1", "h4", "")
	if err != nil || len(st.Operations) != 0 {
		t.Fatalf("empty host: %+v %v", st, err)
	}
	if v := ops.maintenanceView(&HostMaintenanceRecord{}); !v.Ready {
		t.Fatal("host without operations not ready")
	}
}
