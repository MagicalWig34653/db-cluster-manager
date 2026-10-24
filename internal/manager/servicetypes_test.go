// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Fakes only: fake agents, a fake Swarm connector and an in-process store.

func galeraInput(t *testing.T) api.ServiceInput {
	t.Helper()
	r := createReq()
	raw, err := json.Marshal(service.GaleraSpec{Size: r.Size, Image: r.Image, Network: r.Network,
		RootPasswordSecret: r.RootPasswordSecret, Resources: r.Resources})
	if err != nil {
		t.Fatal(err)
	}
	return api.ServiceInput{Swarm: r.Swarm, Name: r.Name, Type: service.TypeGalera, Spec: raw}
}

var tokenRe = regexp.MustCompile(`"token_id":"[^"]+"`)

// normalizedDefinition replaces the generated IDs (operation, cluster,
// node UUIDs, tokens) so two definitions can be compared byte for byte.
func normalizedDefinition(t *testing.T, rec OperationRecord) string {
	t.Helper()
	raw, err := json.Marshal(rec.Definition)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(raw), rec.ID, "OP")
	s = strings.ReplaceAll(s, rec.Cluster, "CLUSTER")
	for i, n := range rec.Nodes {
		s = strings.ReplaceAll(s, n.NodeUUID, "UUID"+string(rune('a'+i)))
		s = strings.ReplaceAll(s, n.NodeUUID[:8], "U8"+string(rune('a'+i)))
	}
	return tokenRe.ReplaceAllString(s, `"token_id":"TOKEN"`)
}

func TestGaleraServiceCreatesTheSameOperationAsCreateCluster(t *testing.T) {
	_, direct, _ := serviceFixture(t)
	directRec, err := direct.Ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	l, svcs, _ := serviceFixture(t)
	ctx := context.Background()
	// The preview changes nothing and shows the exact agent steps.
	pv, err := svcs.PreviewService(ctx, adminSubject, galeraInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Type != service.TypeGalera || pv.RequiresApproval || !pv.ProvisionalIDs || len(pv.AgentSteps) != len(directRec.Definition.Steps) {
		t.Fatalf("preview %+v", pv)
	}
	if recs, _ := List[ClusterRecord](l.Store(), CollClusters); len(recs) != 0 {
		t.Fatal("preview stored a cluster")
	}
	started, err := svcs.CreateService(ctx, adminSubject, galeraInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if started.Operation.Kind != directRec.Kind || started.Approval.Digest != "" {
		t.Fatalf("%+v", started)
	}
	viaType := opRecord(t, l, started.Operation.ID)
	if a, b := normalizedDefinition(t, directRec), normalizedDefinition(t, viaType); a != b {
		t.Fatalf("definitions differ\ndirect %s\ntype   %s", a, b)
	}
	cl, ok, _ := Get[ClusterRecord](l.Store(), CollClusters, started.Service)
	if !ok || cl.Value.Name != "shop" || cl.Value.Size != 3 {
		t.Fatalf("cluster record %+v", cl.Value)
	}
	if len(started.Plan.AgentSteps) != len(viaType.Definition.Steps) || started.Plan.AgentSteps[0].ID != viaType.Definition.Steps[0].ID {
		t.Fatalf("expert view does not match the stored definition")
	}

	// The cluster is listed as a galera service.
	list, err := svcs.Services(ctx)
	if err != nil || len(list) != 1 || list[0].Type != service.TypeGalera || list[0].ID != started.Service || list[0].Health == nil {
		t.Fatalf("%+v %v", list, err)
	}
	// Updates and generic galera paths are refused.
	if _, err := svcs.PreviewServiceUpdate(ctx, adminSubject, started.Service, api.ServiceUpdateInput{Spec: galeraInput(t).Spec}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("galera update: %v", err)
	}
}

func TestGaleraServiceKeepsClusterAuthorizationAndApproval(t *testing.T) {
	l, svcs, agents := serviceFixture(t)
	ctx := context.Background()
	// A role with every service permission but no cluster permission can
	// neither create nor delete a galera service.
	pol := svcs.Ops.Authorizer.Policy()
	if err := pol.AddRole(authz.Role{Name: "svc_admin", Permissions: []authz.Permission{{Type: authz.ResourceService, Action: authz.ActionAny}}}); err != nil {
		t.Fatal(err)
	}
	svcAdmin := authz.Subject{Kind: authz.SubjectUser, ID: "svc-admin"}
	if err := pol.Bind(authz.Binding{Subject: svcAdmin, Role: "svc_admin", Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { _, err := svcs.PreviewService(ctx, svcAdmin, galeraInput(t)); return err },
		func() error { _, err := svcs.CreateService(ctx, svcAdmin, galeraInput(t)); return err },
	} {
		if err := f(); !errors.Is(err, api.ErrForbidden) {
			t.Fatalf("service role on galera: %v", err)
		}
	}
	started, err := svcs.CreateService(ctx, adminSubject, galeraInput(t))
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, started.Operation.ID, runner.StatusSucceeded)
	if _, err := svcs.DeleteService(ctx, svcAdmin, started.Service, false); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("service role deleted a cluster: %v", err)
	}
	// Deletion is the cluster deletion with its approval.
	pv, err := svcs.PreviewServiceDelete(ctx, adminSubject, started.Service, false)
	if err != nil || pv.Type != service.TypeGalera || !pv.RequiresApproval || len(pv.AgentSteps) == 0 {
		t.Fatalf("delete preview %+v %v", pv, err)
	}
	before := len(agents.commands())
	del, err := svcs.DeleteService(ctx, adminSubject, started.Service, false)
	if err != nil {
		t.Fatal(err)
	}
	if del.Operation.Kind != OpClusterDelete || len(del.Approval.Digest) != 64 {
		t.Fatalf("%+v", del)
	}
	waitOpStatus(t, l, del.Operation.ID, runner.StatusPaused)
	if len(agents.commands()) != before {
		t.Fatal("cluster deletion ran before the approval")
	}
	for _, st := range opRecord(t, l, del.Operation.ID).Definition.Steps {
		if !slices.ContainsFunc(st.Preconditions, func(c runner.Condition) bool { return c.Kind == "approved" }) {
			t.Errorf("step %s does not wait for the approval", st.ID)
		}
	}
}

// svcConn is a fake Swarm connector whose service list the test changes.
type svcConn struct {
	refConn
	mu   sync.Mutex
	list []agent.Object
}

func (c *svcConn) ListByLabel(_ context.Context, k agent.ObjectKind, key, value string) ([]agent.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []agent.Object
	for _, o := range c.list {
		if k == agent.KindService && o.Labels[key] == value {
			out = append(out, o)
		}
	}
	return out, nil
}

func (c *svcConn) set(objs ...agent.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = objs
}

func tasks(id string, running, desired uint64, update string) agent.Object {
	return agent.Object{Kind: agent.KindService, ID: "sw-" + id, Name: "dbcm-svc-web", Labels: map[string]string{agent.LabelCluster: id},
		Replicas: desired, RunningTasks: &running, UpdateState: update}
}

func updateFixture(t *testing.T) (*Service, *Services, *fakeAgents, *svcConn) {
	t.Helper()
	agents := newFakeAgents()
	conn := &svcConn{}
	rc := &RoutingChecker{Cluster: okChecker{}}
	c := newClusterWith(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, rc },
		func(_ string, s *Service) {
			rc.Store = s.Store()
			rc.Ops = &Operations{Svc: s, Swarms: &Swarms{Dial: func(string) (swarm.Connector, error) { return conn, nil }}}
		})
	l := c.leader()
	ops := seed(t, l, 3)
	return l, &Services{Ops: ops}, agents, conn
}

func TestServiceUpdateRollbackAndHealthGate(t *testing.T) {
	l, svcs, agents, conn := updateFixture(t)
	ctx := context.Background()
	ctl := &Control{Ops: svcs.Ops, Swarms: &Swarms{}}
	in := containerInput(t, func(c *service.ContainerSpec) { c.Volumes, c.Replicas = nil, 2; c.Placement.HostID = "" })
	started, err := svcs.CreateService(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, started.Operation.ID, started.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, started.Operation.ID, runner.StatusSucceeded)
	id := started.Service

	next := containerInput(t, func(c *service.ContainerSpec) {
		c.Volumes, c.Replicas, c.Image = nil, 2, "nginx:1.27.3"
		c.Placement.HostID = ""
	})
	upd := api.ServiceUpdateInput{Spec: next.Spec, Strategy: api.ServiceUpdateStrategy{Order: "start-first"}}
	if _, err := svcs.UpdateService(ctx, operatorSubject, id, upd); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("operator update: %v", err)
	}
	if _, err := svcs.UpdateService(ctx, adminSubject, id, api.ServiceUpdateInput{Rollback: true}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("rollback without a previous spec: %v", err)
	}
	pv, err := svcs.PreviewServiceUpdate(ctx, adminSubject, id, upd)
	if err != nil || pv.Kind != "update" || pv.Rollback == nil || len(pv.AgentSteps) != 3 || !pv.ProvisionalIDs {
		t.Fatalf("preview %+v %v", pv, err)
	}
	if pv.AgentSteps[0].Command != string(agent.KindUpdateService) || pv.AgentSteps[2].Command != "" ||
		!slices.Contains(pv.AgentSteps[2].Preconditions, runner.CondServiceHealthy+" "+id) {
		t.Fatalf("expert view %+v", pv.AgentSteps)
	}

	// The health gate holds only when every desired task runs.
	conn.set(tasks(id, 1, 2, "updating"))
	u, err := svcs.UpdateService(ctx, adminSubject, id, upd)
	if err != nil {
		t.Fatal(err)
	}
	if u.Operation.Kind != runner.OpServiceUpdate || len(u.Approval.Digest) != 64 {
		t.Fatalf("%+v", u)
	}
	waitOpStatus(t, l, u.Operation.ID, runner.StatusPaused)
	n := len(agents.commands())
	if _, err := ctl.ApproveOperation(ctx, adminSubject, u.Operation.ID, u.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "update and await sent", func() bool {
		var kinds []agent.Kind
		for _, c := range agents.commands()[n:] {
			kinds = append(kinds, c.Cmd.Kind)
		}
		return slices.Equal(kinds, []agent.Kind{agent.KindUpdateService, agent.KindAwaitService})
	})
	var ua agent.UpdateServiceArgs
	for _, c := range agents.commands() {
		if c.Cmd.Kind == agent.KindUpdateService {
			_ = json.Unmarshal(c.Cmd.Args, &ua)
		}
	}
	if ua.Spec.Image != "nginx:1.27.3" || ua.Spec.UpdateConfig == nil || ua.Spec.UpdateConfig.Order != agent.UpdateStartFirst ||
		ua.Spec.UpdateConfig.FailureAction != agent.FailureActionRollback {
		t.Fatalf("update args %+v", ua.Spec)
	}
	if rec, _, _ := Get[ServiceRecord](l.Store(), CollServices, id); rec.Value.UpdateOp != u.Operation.ID ||
		strings.Contains(string(rec.Value.Spec), "1.27.3") {
		t.Fatalf("spec changed before success: %+v", rec.Value)
	}
	// One of two tasks runs: the gate pauses the operation instead of
	// succeeding; once every task runs a resume finishes it.
	waitFor(t, "health gate pause", func() bool {
		v := opRecord(t, l, u.Operation.ID).View
		return v != nil && v.Status == runner.StatusPaused && v.PausedStep == "03-check-health-web" && strings.Contains(v.Reason, "1 of 2 tasks run")
	})
	conn.set(tasks(id, 2, 2, "completed"))
	if err := svcs.Ops.Resume(adminSubject, u.Operation.ID); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, u.Operation.ID, runner.StatusSucceeded)
	waitFor(t, "spec recorded", func() bool {
		rec, _, _ := Get[ServiceRecord](l.Store(), CollServices, id)
		return rec.Value.UpdateOp == "" && strings.Contains(string(rec.Value.Spec), "1.27.3") && strings.Contains(string(rec.Value.PreviousSpec), "1.27.2")
	})

	// The rollback plans the update back to the previous spec.
	rb, err := svcs.PreviewServiceUpdate(ctx, adminSubject, id, api.ServiceUpdateInput{Rollback: true})
	if err != nil || !strings.Contains(rb.Steps[0].Note, "from image nginx:1.27.3 to nginx:1.27.2") {
		t.Fatalf("rollback %+v %v", rb, err)
	}
	if info, _ := svcs.Service(ctx, id); !info.Rollbackable {
		t.Fatal("not rollbackable")
	}
}

func TestServiceHealthGateFailsClosedWithoutSwarm(t *testing.T) {
	_, svcs, _ := serviceFixture(t)
	rc := &RoutingChecker{Store: svcs.Ops.Svc.Store(), Ops: &Operations{Svc: svcs.Ops.Svc}}
	if _, err := Put(rc.Store, CollServices, "svc-x", ServiceRecord{ID: "svc-x", Swarm: "sw1", Name: "web", Type: "container",
		Spec: json.RawMessage(`{"image":"nginx:1.27.2"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	ok, reason, err := rc.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondServiceHealthy, Target: "svc-x"})
	if err != nil || ok || !strings.Contains(reason, "not observable") {
		t.Fatalf("%v %q %v", ok, reason, err)
	}
}

func TestServiceHealthCollectorNotifiesTransitions(t *testing.T) {
	l, _, _, conn := updateFixture(t)
	if _, err := Put(l.Store(), CollServices, "svc-h", ServiceRecord{ID: "svc-h", Swarm: "sw1", Name: "web", Type: "container",
		Spec: json.RawMessage(`{"image":"nginx:1.27.2","replicas":2}`)}, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []notify.Event
	hc := ServiceHealthCollector{Swarms: &Swarms{Dial: func(string) (swarm.Connector, error) { return conn, nil }},
		Notify: func(_ context.Context, e notify.Event) bool {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
			return true
		}}
	collect := func() api.ServiceHealth {
		t.Helper()
		if err := hc.CollectOnce(context.Background(), l.Current()); err != nil {
			t.Fatal(err)
		}
		rec, _, _ := Get[ServiceRecord](l.Store(), CollServices, "svc-h")
		return *apiServiceHealth(rec.Value.Observed, l.cfg.Now())
	}
	conn.set(tasks("svc-h", 2, 2, ""))
	if h := collect(); h.State != "healthy" || h.Running != 2 {
		t.Fatalf("%+v", h)
	}
	conn.set(tasks("svc-h", 0, 2, ""))
	if h := collect(); h.State != "unhealthy" {
		t.Fatalf("%+v", h)
	}
	conn.set(tasks("svc-h", 2, 2, "rollback_completed"))
	if h := collect(); h.State != "degraded" || !strings.Contains(strings.Join(h.Reasons, " "), "rollback_completed") {
		t.Fatalf("rolled back update: %+v", h)
	}
	conn.set()
	if h := collect(); h.State != "unknown" {
		t.Fatalf("service gone: %+v", h)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Kind != KindServiceUnhealthy || events[0].Details["service"] != "svc-h" {
		t.Fatalf("events %+v", events)
	}
}
