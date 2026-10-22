// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const gib = int64(1) << 30

var (
	adminSubject = authz.Subject{ID: "admin", Kind: authz.SubjectUser}
	nobody       = authz.Subject{ID: "nobody", Kind: authz.SubjectUser}
)

// seed writes settings, one Swarm with n enrolled hosts and an
// administrator binding, and returns an Operations for s.
func seed(t *testing.T, s *Service, hosts int) *Operations {
	t.Helper()
	st := s.Store()
	if _, err := Put(st, CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm", AgentImage: "agent:1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollSwarms, "sw1", SwarmRecord{ID: "sw1", Name: "lab"}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= hosts; i++ {
		id := fmt.Sprintf("h%d", i)
		h := HostRecord{Swarm: "sw1", NodeID: id, State: "ready", Availability: "active", Enrollment: EnrollEnrolled,
			NanoCPUs: 4e9, MemoryBytes: 8 * gib}
		if _, err := Put(st, CollHosts, HostKey("sw1", id), h, nil); err != nil {
			t.Fatal(err)
		}
	}
	return opsFor(t, s)
}

func opsFor(t *testing.T, s *Service) *Operations {
	t.Helper()
	a, err := LoadAuth(s.Store(), testKey, testAuth)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Policy.Bindings(adminSubject)) == 0 {
		if err := a.Policy.Bind(authz.Binding{Subject: adminSubject, Role: authz.RoleAdministrator, Scope: authz.Global}); err != nil {
			t.Fatal(err)
		}
	}
	return &Operations{Svc: s, Authorizer: a.Authorizer, StepTimeout: time.Second, MaxRetries: 1}
}

func createReq() CreateClusterRequest {
	return CreateClusterRequest{
		Swarm: "sw1", Name: "shop", Size: 3,
		Image:   "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873",
		Network: "dbcm-shop-galera", RootPasswordSecret: "dbcm-shop-root-pw",
		Resources: galera.Resources{
			Limits:       galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib},
			Reservations: galera.ResourceAmount{MilliCPU: 500, MemoryBytes: gib},
		},
	}
}

func opRecord(t *testing.T, s *Service, id string) OperationRecord {
	t.Helper()
	r, ok, err := Get[OperationRecord](s.Store(), CollOperations, id)
	if err != nil || !ok {
		t.Fatalf("operation %s: %v %v", id, ok, err)
	}
	return r.Value
}

func waitOpStatus(t *testing.T, s *Service, id string, want runner.Status) OperationRecord {
	t.Helper()
	var rec OperationRecord
	waitFor(t, "operation "+string(want), func() bool {
		r, ok, _ := Get[OperationRecord](s.Store(), CollOperations, id)
		rec = r.Value
		return ok && rec.View != nil && rec.View.Status == want
	})
	return rec
}

func TestCreateClusterRunsToCompletion(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 3, func(_ string, cfg *Config) { cfg.Agents = agents; cfg.Checker = okChecker{} })
	l := c.leader()
	ops := seed(t, l, 3)
	rec, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	done := waitOpStatus(t, l, rec.ID, runner.StatusSucceeded)
	if done.EndedAt.IsZero() {
		t.Fatal("end time not recorded")
	}
	cl, ok, _ := Get[ClusterRecord](l.Store(), CollClusters, rec.Cluster)
	if !ok || len(cl.Value.Nodes) != 3 {
		t.Fatalf("cluster record %+v", cl)
	}
	waitFor(t, "service IDs recorded", func() bool {
		cl, _, _ = Get[ClusterRecord](l.Store(), CollClusters, rec.Cluster)
		return len(cl.Value.ServiceID) == 3
	})
	for _, cmd := range agents.commands() {
		if cmd.Cmd.Term != l.Current().Term {
			t.Fatalf("command with term %d, leader term %d", cmd.Cmd.Term, l.Current().Term)
		}
	}
	// The view is replicated to followers for the UI.
	f := c.follower(l)
	waitFor(t, "view on follower", func() bool {
		r, ok, _ := Get[OperationRecord](f.Store(), CollOperations, rec.ID)
		return ok && r.Value.View != nil && r.Value.View.Status == runner.StatusSucceeded
	})
	ops2, _ := Inventory{Store: f.Store()}.Operations(context.Background())
	if len(ops2) != 1 || ops2[0].State != "succeeded" || ops2[0].Cluster != rec.Cluster {
		t.Fatalf("activity %+v", ops2)
	}
	// A second cluster with the same name in the Swarm is refused.
	if _, err := ops.CreateCluster(adminSubject, createReq()); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestCreateClusterDeniedWithoutPermission(t *testing.T) {
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = newFakeAgents() })
	l := c.leader()
	ops := seed(t, l, 3)
	_, err := ops.CreateCluster(nobody, createReq())
	var denied *authz.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("got %v", err)
	}
	if docs, _ := l.Store().RawList(CollOperations); len(docs) != 0 {
		t.Fatal("operation stored although denied")
	}
	recs, _ := AuditRecords(l.Store())
	found := false
	for _, r := range recs {
		if r.Actor == nobody && r.Outcome == authz.OutcomeDenied {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied decision not audited: %+v", recs)
	}
}

func TestOperationsRequireLeadership(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	seed(t, l, 3)
	f := c.follower(l)
	ops := &Operations{Svc: f, Authorizer: authz.NewAuthorizer(authz.NewPolicy(), &authz.MemorySink{}, nil)}
	if _, err := ops.CreateCluster(adminSubject, createReq()); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower started an operation: %v", err)
	}
}

// TestFailoverTakesOverAndResumes blocks the first create-service command,
// closes the leader while it is in flight and checks that the new leader
// adopts the operation, re-queries the in-doubt step under its own term
// and completes it, and that the old leader sends nothing after stepping
// down.
func TestFailoverTakesOverAndResumes(t *testing.T) {
	agents := newFakeAgents()
	entered := make(chan struct{}, 1)
	blocked := true
	agents.block = func(ctx context.Context, cmd agent.Command) error {
		agents.mu.Lock()
		b := blocked && cmd.Kind == agent.KindCreateService
		if b {
			blocked = false
		}
		agents.mu.Unlock()
		if !b {
			return nil
		}
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	c := newCluster(t, 3, func(_ string, cfg *Config) {
		cfg.Agents, cfg.Checker, cfg.AutoResume = agents, okChecker{}, true
	})
	old := c.leader()
	ops := seed(t, old, 3)
	rec, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	oldTerm := old.Current().Term
	c.close(old)
	sentByOld := len(agents.commands())
	next := c.leader(old.cfg.NodeID)
	done := waitOpStatus(t, next, rec.ID, runner.StatusSucceeded)
	if done.View.Term <= oldTerm {
		t.Fatalf("finished under term %d, old term %d", done.View.Term, oldTerm)
	}
	cmds := agents.commands()
	for _, cmd := range cmds[sentByOld:] {
		if cmd.Cmd.Term == oldTerm {
			t.Fatalf("command with the old term after step-down: %+v", cmd)
		}
	}
	adopted := false
	for _, e := range done.View.Timeline {
		if e.Type == runner.EventAdopt {
			adopted = true
		}
	}
	if !adopted {
		t.Fatal("operation was not adopted by the new leader")
	}
}

func TestFencedAgentsRefuseOutsideTerm(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents })
	l := c.leader().Current()
	if _, err := l.Agents.Send(context.Background(), "h1", agent.Command{Term: l.Term + 1, OpID: "o", StepID: "s"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("wrong term: %v", err)
	}
	l.cancel()
	if _, err := l.Agents.Send(context.Background(), "h1", agent.Command{Term: l.Term, OpID: "o", StepID: "s"}); !errors.Is(err, fencing.ErrStaleTerm) {
		t.Fatalf("ended leadership: %v", err)
	}
	if len(agents.commands()) != 0 {
		t.Fatal("a command reached the agent")
	}
	if l.Go(func(context.Context) {}) {
		t.Fatal("goroutine started on ended leadership")
	}
}

// replaceFixture stores a running 3-node cluster whose slot n3 failed and
// a spare host h4.
func replaceFixture(t *testing.T, s *Service, healthy int) (*Operations, ClusterRecord) {
	t.Helper()
	ops := seed(t, s, 4)
	now := s.cfg.Now()
	cl := ClusterRecord{ID: "clu-1", Swarm: "sw1", Name: "shop", Engine: "mariadb-galera", Size: 3, LastKnownSize: 3,
		Spec: galera.Cluster{InstallationID: "inst-1", ClusterID: "clu-1", Name: "shop", Image: createReq().Image,
			Network: "dbcm-shop-galera", RootPasswordSecret: "dbcm-shop-root-pw", Members: []string{"n1", "n2", "n3"}},
		Resources: createReq().Resources,
		ServiceID: map[string]string{"n1": "svc1", "n2": "svc2", "n3": "svc3"},
		Health:    map[string]NodeObservation{},
	}
	for i, slot := range []string{"n1", "n2", "n3"} {
		cl.Nodes = append(cl.Nodes, galera.Node{Slot: slot, NodeUUID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1),
			HostID: fmt.Sprintf("h%d", i+1), Volume: galera.VolumeName("shop", slot, fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1)),
			Resources: createReq().Resources})
	}
	for i, slot := range []string{"n1", "n2"} {
		h := galera.NodeHealth{State: galera.HealthHealthy, LocalState: "Synced", ClusterStatus: "Primary", Ready: true, Connected: true,
			ClusterSize: 2, StateUUID: "11111111-1111-1111-1111-111111111111"}
		if i >= healthy {
			h = galera.NodeHealth{State: galera.HealthUnhealthy}
		}
		cl.Health[slot] = NodeObservation{Health: h, ObservedAt: now}
	}
	cl.Health["n3"] = NodeObservation{Health: galera.NodeHealth{State: galera.HealthUnhealthy}, ObservedAt: now}
	if _, err := Put(s.Store(), CollClusters, cl.ID, cl, nil); err != nil {
		t.Fatal(err)
	}
	return ops, cl
}

func TestReplaceNodeRefusesWithoutQuorum(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents })
	l := c.leader()
	ops, _ := replaceFixture(t, l, 1) // only one of three healthy: no majority
	_, err := ops.ReplaceNode(adminSubject, "clu-1", "n3")
	if !errors.Is(err, galera.ErrRequiresGuidedRecovery) {
		t.Fatalf("got %v", err)
	}
	if docs, _ := l.Store().RawList(CollOperations); len(docs) != 0 || len(agents.commands()) != 0 {
		t.Fatal("replacement started without quorum")
	}
	if _, err := ops.ReplaceNode(nobody, "clu-1", "n3"); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized replacement: %v", err)
	}
}

func TestReplaceNodeWithQuorumUpdatesDesiredStateOnSuccess(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, okChecker{} })
	l := c.leader()
	ops, before := replaceFixture(t, l, 2)
	rec, err := ops.ReplaceNode(adminSubject, "clu-1", "n3")
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, rec.ID, runner.StatusSucceeded)
	var after ClusterRecord
	waitFor(t, "desired state updated", func() bool {
		cr, _, _ := Get[ClusterRecord](l.Store(), CollClusters, "clu-1")
		after = cr.Value
		return after.ServiceID["n3"] != "" && after.ServiceID["n3"] != "svc3"
	})
	var oldN3, newN3 galera.Node
	for i := range before.Nodes {
		if before.Nodes[i].Slot == "n3" {
			oldN3, newN3 = before.Nodes[i], after.Nodes[i]
		}
	}
	if newN3.NodeUUID == oldN3.NodeUUID || newN3.HostID == oldN3.HostID {
		t.Fatalf("replacement kept the old identity or host: %+v", newN3)
	}
	// A second replacement while one is unfinished is refused.
	if _, err := Put(l.Store(), CollOperations, "op-x", OperationRecord{ID: "op-x", Cluster: "clu-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.ReplaceNode(adminSubject, "clu-1", "n3"); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("concurrent replacement: %v", err)
	}
}

func TestDiagnoseOutageIsReadOnly(t *testing.T) {
	agents := newFakeAgents()
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents })
	l := c.leader()
	ops, _ := replaceFixture(t, l, 0)
	uuid := "11111111-1111-1111-1111-111111111111"
	ev := []galera.NodeInfo{
		{Slot: "n1", Grastate: &galera.Grastate{UUID: uuid, Seqno: 10}},
		{Slot: "n2", Grastate: &galera.Grastate{UUID: uuid, Seqno: 12}},
		{Slot: "n3", Grastate: &galera.Grastate{UUID: uuid, Seqno: 11}},
	}
	before, _ := AuditRecords(l.Store())
	d, err := ops.DiagnoseOutage(adminSubject, "clu-1", ev)
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate != "n2" || !d.RequiresApproval {
		t.Fatalf("diagnosis %+v", d)
	}
	if docs, _ := l.Store().RawList(CollOperations); len(docs) != 0 || len(agents.commands()) != 0 {
		t.Fatal("diagnosis started something")
	}
	if after, _ := AuditRecords(l.Store()); len(after) != len(before) {
		t.Fatal("read-only diagnosis wrote audit records")
	}
	if _, err := ops.DiagnoseOutage(nobody, "clu-1", ev); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized diagnosis: %v", err)
	}
	if _, err := runner.FromGaleraPlan(d.Plan, runner.GaleraBindings{OpID: "x", InstallationID: "i", VolumeBase: "/v"}); err == nil {
		t.Fatal("recovery plan became executable")
	}
}
