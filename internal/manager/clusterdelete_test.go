// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

var operatorSubject = authz.Subject{ID: "operator", Kind: authz.SubjectUser}

// TestDeleteDiscardsFailedCreateAndReleasesName: lab E4 found that a failed
// create kept its name and planned capacity and its half-created objects;
// the development run had to wipe the manager state by hand. The deletion
// needs the delete permission, waits for a digest-bound approval, removes
// the services and volumes through the agents (missing or unmarked ones
// count as removed) and then releases the name.
func TestDeleteDiscardsFailedCreateAndReleasesName(t *testing.T) {
	agents := newFakeAgents()
	agents.refuse = func(cmd agent.Command) bool { return cmd.Kind == agent.KindCreateVolume }
	rc := &RoutingChecker{Cluster: okChecker{}}
	c := newClusterWith(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, rc },
		func(_ string, s *Service) { rc.Store, rc.Ops = s.Store(), &Operations{Svc: s} })
	l := c.leader()
	ops := seed(t, l, 3)
	ops.MaxRetries = 3
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	create, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, create.ID, runner.StatusFailed)
	if _, err := ops.CreateCluster(adminSubject, createReq()); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("name still taken after the failed create: %v", err)
	}
	clusterID := create.Cluster

	ctl := &Control{Ops: ops, Swarms: &Swarms{}}
	ctx := context.Background()
	for _, who := range []authz.Subject{nobody, operatorSubject} {
		if _, err := ctl.DeleteCluster(ctx, who, clusterID); !errors.Is(err, api.ErrForbidden) {
			t.Fatalf("%s: %v", who.ID, err)
		}
	}
	if _, err := ctl.DeleteCluster(ctx, adminSubject, "nope"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	d, err := ctl.DeleteCluster(ctx, adminSubject, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != OpClusterDelete || len(d.Approval.Digest) != 64 {
		t.Fatalf("%+v", d)
	}
	// The failed create was aborted (it held the cluster lock).
	waitOpStatus(t, l, create.ID, runner.StatusAborted)
	// Nothing is removed before the approval.
	waitOpStatus(t, l, d.ID, runner.StatusPaused)
	sentFor := func(op string) []sent {
		var out []sent
		for _, s := range agents.commands() {
			if s.Cmd.OpID == op {
				out = append(out, s)
			}
		}
		return out
	}
	if n := len(sentFor(d.ID)); n != 0 {
		t.Fatalf("%d commands before the approval", n)
	}
	rec := opRecord(t, l, d.ID)
	var kinds []string
	for _, st := range rec.Definition.Steps {
		kinds = append(kinds, string(st.Command))
		if !slices.ContainsFunc(st.Preconditions, func(c runner.Condition) bool { return c.Kind == dbbackup.CondApproved }) {
			t.Errorf("step %s does not wait for the approval", st.ID)
		}
		if st.Command == agent.KindRemoveVolume {
			var a agent.RemoveVolumeArgs
			if err := json.Unmarshal(st.Args, &a); err != nil || !a.MissingOK || !a.AllowUnmarked || a.Expected.ClusterID != clusterID ||
				a.Expected.InstallationID != "inst-1" || !strings.HasPrefix(a.VolumeRoot, "/srv/dbcm/dbcm-shop-") ||
				!slices.Equal(st.RetryCodes, []agent.Code{agent.CodeVolumeInUse}) {
				t.Errorf("remove-volume %+v %v (%v)", a, st.RetryCodes, err)
			}
		}
	}
	want := []string{"remove-cluster-service", "remove-cluster-service", "remove-cluster-service", "remove-volume", "remove-volume", "remove-volume"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("steps %v", kinds)
	}
	if _, err := ctl.ApproveOperation(ctx, operatorSubject, d.ID, d.Approval.Digest); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("operator approved a deletion: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, d.ID, d.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, d.ID, runner.StatusSucceeded)
	waitFor(t, "cluster record deleted", func() bool {
		_, ok, _ := Get[ClusterRecord](l.Store(), CollClusters, clusterID)
		return !ok
	})
	var removed []string
	for _, s := range sentFor(d.ID) {
		if s.Cmd.Kind == agent.KindRemoveClusterService {
			var a agent.RemoveClusterServiceArgs
			_ = json.Unmarshal(s.Cmd.Args, &a)
			if a.ClusterID != clusterID {
				t.Fatalf("service removal without the cluster label check: %+v", a)
			}
			removed = append(removed, s.Target+":"+a.ServiceName)
		}
	}
	slices.Sort(removed)
	if len(removed) != 3 || !strings.HasSuffix(removed[0], "dbcm-shop-n1") {
		t.Fatalf("removed %v", removed)
	}
	// Name and capacity are free again.
	if _, err := ops.CreateCluster(adminSubject, createReq()); err != nil {
		t.Fatalf("create after the deletion: %v", err)
	}
}

func TestDeleteRefusesWhileAnOperationRuns(t *testing.T) {
	env := newBackupEnv(t, true)
	ref, err := env.b.StartBackup(context.Background(), adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.ops.DeleteCluster(adminSubject, "clu-1"); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("delete during a backup: %v", err)
	}
	if !errors.Is(MapCommandError(runner.ErrBusy), api.ErrBusy) {
		t.Fatal("busy not mapped")
	}
	_ = ref
}

// TestDeletePlanCoversJobsInspectionsAndReplacementVolumes: the deletion
// also removes the job of a backup that did not finish, an inspection
// instance of the cluster that was not torn down (with its init secret),
// and the new volume of a replacement that did not finish.
func TestDeletePlanCoversJobsInspectionsAndReplacementVolumes(t *testing.T) {
	env := newBackupEnv(t, true)
	st := env.svc.Store()
	ended := env.clock.Now().Add(-time.Hour)
	env.completedBackup(t, "b1", ended)
	// b2: failed backup from n2, its job may still exist.
	if _, err := Put(st, CollOperations, "op-b2", OperationRecord{ID: "op-b2", Kind: dbbackup.OpBackup, Swarm: "sw1", Cluster: "clu-1",
		View: &runner.View{ID: "op-b2", Status: runner.StatusAborted}, RequestedAt: ended, EndedAt: ended}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollBackups, "b2", BackupRecord{ID: "b2", Cluster: "clu-1", Swarm: "sw1", OpID: "op-b2", Source: "n2",
		Method: dbbackup.MethodPhysical, Objects: []string{"x"}}, nil); err != nil {
		t.Fatal(err)
	}
	// An aborted replacement of n3 planned a new volume.
	cur, _, _ := Get[ClusterRecord](st, CollClusters, "clu-1")
	repl := cur.Value.Nodes[2]
	repl.NodeUUID, repl.HostID = "00000000-0000-4000-8000-0000000000aa", "h4"
	repl.Volume = "dbcm-shop-n3-000000aa"
	if _, err := Put(st, CollOperations, "op-rep", OperationRecord{ID: "op-rep", Kind: "galera/replace", Swarm: "sw1", Cluster: "clu-1",
		View: &runner.View{ID: "op-rep", Status: runner.StatusAborted}, RequestedAt: ended, EndedAt: ended,
		Nodes: []galera.Node{repl}}, nil); err != nil {
		t.Fatal(err)
	}
	// An inspection whose operation ended and that was not torn down.
	insp := dbbackup.InspectionSession{ID: "iabc", BackupID: "b1", ClusterID: "inspect-iabc", NodeUUID: "00000000-0000-4000-8000-0000000000bb",
		HostID: "h4", Volume: "dbcm-inspect-iabc-00000000", VolumeRoot: "/srv/dbcm/dbcm-inspect-iabc-00000000",
		ServiceName: "dbcm-inspect-iabc", JobName: "dbcm-job-b1-inspect-iabc", Network: "dbcm-inspect-net", Port: 3307}
	out, _ := json.Marshal(map[string]string{"id": "svc-insp"})
	if _, err := Put(st, CollOperations, "op-insp", OperationRecord{ID: "op-insp", Kind: dbbackup.OpInspection, Swarm: "sw1", Cluster: "clu-1",
		View: &runner.View{ID: "op-insp", Status: runner.StatusSucceeded, Steps: []runner.StepView{{ID: "07-create-instance",
			LastResult: &agent.Result{Status: agent.StatusSucceeded, Output: out}}}}, RequestedAt: ended, EndedAt: ended}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollInspections, "iabc", InspectionRecord{Session: insp, Cluster: "clu-1", Swarm: "sw1", OpID: "op-insp",
		InitSecret: "dbcm-inspect-iabc-init"}, nil); err != nil {
		t.Fatal(err)
	}
	d, err := env.ops.DeleteCluster(adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range d.Operation.Definition.Steps {
		got = append(got, string(s.Command)+"@"+s.Target)
	}
	want := []string{
		"remove-cluster-service@h1", "remove-cluster-service@h2", "remove-cluster-service@h3",
		"remove-job@h2",
		"remove-job@h4", "remove-service@h4", "remove-secret@h4", "remove-volume@h4",
		"remove-volume@h1", "remove-volume@h2", "remove-volume@h3", "remove-volume@h4",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("steps\n%v\nwant\n%v", got, want)
	}
	if !slices.Contains(d.Operation.Definition.Locks, "inspection/iabc") {
		t.Fatalf("locks %v", d.Operation.Definition.Locks)
	}
	ir, _, _ := Get[InspectionRecord](st, CollInspections, "iabc")
	cl, _, _ := Get[ClusterRecord](st, CollClusters, "clu-1")
	if ir.Value.TeardownOp != d.Operation.ID || cl.Value.DeleteOp != d.Operation.ID {
		t.Fatalf("records not marked: %q %q", ir.Value.TeardownOp, cl.Value.DeleteOp)
	}
}
