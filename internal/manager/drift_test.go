// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
)

func driftServices(cl ClusterRecord) []agent.Object {
	one := uint64(1)
	var out []agent.Object
	for _, n := range cl.Nodes {
		out = append(out, agent.Object{Kind: agent.KindService, ID: cl.ServiceID[n.Slot], Name: galera.ServiceName(cl.Name, n.Slot),
			Labels:      map[string]string{agent.LabelInstallation: "inst-1", galera.LabelCluster: cl.ID},
			Constraints: []string{"node.id==" + n.HostID}, Replicas: 1, RunningTasks: &one})
	}
	return out
}

func findingKinds(rep api.DriftReport) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.Kind+":"+f.Slot+f.Service)
	}
	return out
}

func TestCompareDrift(t *testing.T) {
	c := newCluster(t, 1, nil)
	l := c.leader()
	_, cl := replaceFixture(t, l, 2)
	now := l.cfg.Now()
	svcs := driftServices(cl)
	if rep := CompareDrift(cl, svcs, "inst-1", nil, false, now); !rep.InSync || len(rep.Findings) != 0 {
		t.Fatalf("in sync: %v", findingKinds(rep))
	}
	zero := uint64(0)
	svcs = driftServices(cl)
	svcs[0].ID = "svc-other"                            // recreated outside
	svcs[1].Constraints = []string{"node.role==worker"} // moved away from its volume
	svcs[2].Replicas = 0                                // stopped by hand
	svcs = append(svcs, agent.Object{Kind: agent.KindService, ID: "x", Name: "dbcm-shop-n9",
		Labels: map[string]string{galera.LabelCluster: cl.ID, agent.LabelInstallation: "inst-1"}})
	svcs = append(svcs, agent.Object{Kind: agent.KindService, ID: "j", Name: "dbcm-job-b1",
		Labels: map[string]string{galera.LabelCluster: cl.ID, agent.LabelJob: agent.JobBackup}})
	rep := CompareDrift(cl, svcs, "inst-1", nil, false, now)
	got := strings.Join(findingKinds(rep), " ")
	for _, want := range []string{DriftReplacedService + ":n1", DriftPlacementChanged + ":n2", DriftStopped + ":n3", DriftUnexpectedService + ":dbcm-shop-n9"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "dbcm-job") || rep.InSync {
		t.Fatalf("job service reported or in sync: %s", got)
	}
	for _, f := range rep.Findings {
		if f.Action == "" {
			t.Fatalf("finding without action %+v", f)
		}
	}
	// A stopped node on a host in maintenance or during an operation is
	// expected; a missing service and a node without running task are not.
	svcs = driftServices(cl)
	svcs[2].Replicas = 0
	if rep := CompareDrift(cl, svcs, "inst-1", []string{"h3"}, false, now); !rep.InSync {
		t.Fatalf("maintenance: %v", findingKinds(rep))
	}
	svcs = driftServices(cl)[:2]
	svcs[1].RunningTasks = &zero
	svcs[0].Labels[agent.LabelInstallation] = "other"
	got = strings.Join(findingKinds(CompareDrift(cl, svcs, "inst-1", nil, false, now)), " ")
	for _, want := range []string{DriftMissingService + ":n3", DriftNotRunning + ":n2", DriftForeignLabels + ":n1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

// TestDriftDetectorRecordsEventOnChange: the periodic check stores the
// observation and records an event only when the findings change; the API
// check needs cluster read permission and a Swarm connector.
func TestDriftDetectorRecordsEventOnChange(t *testing.T) {
	c := newCluster(t, 1, nil)
	l := c.leader()
	ops, cl := replaceFixture(t, l, 2)
	conn := &refConn{objs: map[agent.ObjectKind][]agent.Object{agent.KindService: driftServices(cl)}}
	sw := &Swarms{Dial: func(string) (swarm.Connector, error) { return conn, nil }}
	ctl := &Control{Ops: ops, Swarms: sw}
	ctx := context.Background()
	rep, err := ctl.ClusterDrift(ctx, adminSubject, "clu-1")
	if err != nil || !rep.InSync {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := ctl.ClusterDrift(ctx, nobody, "clu-1"); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := (&Control{Ops: ops, Swarms: &Swarms{}}).ClusterDrift(ctx, adminSubject, "clu-1"); !errors.Is(err, api.ErrSettingsIncomplete) {
		t.Fatalf("no connector: %v", err)
	}
	events := func() int {
		recs, _ := List[EventRecord](l.Store(), CollEvents)
		n := 0
		for _, r := range recs {
			if strings.Contains(r.Value.Message, "drifted") {
				n++
			}
		}
		return n
	}
	d := DriftDetector{Swarms: sw}
	if err := d.CheckOnce(ctx, l.Current()); err != nil {
		t.Fatal(err)
	}
	conn.objs[agent.KindService] = driftServices(cl)[:2]
	for range 2 {
		if err := d.CheckOnce(ctx, l.Current()); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := Get[ClusterRecord](l.Store(), CollClusters, "clu-1")
	if events() != 1 || got.Value.Drift == nil || got.Value.Drift.Findings != 1 {
		t.Fatalf("events %d drift %+v", events(), got.Value.Drift)
	}
	// Nothing was changed on the Swarm or through the agents.
	if ops, _ := List[OperationRecord](l.Store(), CollOperations); len(ops) != 0 {
		t.Fatalf("drift started operations: %+v", ops)
	}
}
