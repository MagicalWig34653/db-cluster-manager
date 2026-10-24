// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Fakes only: fake agents and an in-process store. No Swarm, Docker or VM.

func containerInput(t *testing.T, mut func(*service.ContainerSpec)) api.ServiceInput {
	t.Helper()
	spec := service.ContainerSpec{
		Image:     "nginx:1.27.2",
		Volumes:   []service.Volume{{Name: "data", Target: "/data"}},
		Placement: service.Placement{HostID: "h1"},
		Resources: service.Resources{Limits: service.ResourceAmount{MilliCPU: 500, MemoryBytes: 256 << 20}},
	}
	if mut != nil {
		mut(&spec)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return api.ServiceInput{Swarm: "sw1", Name: "web", Type: "container", Spec: raw}
}

func serviceFixture(t *testing.T) (*Service, *Services, *fakeAgents) {
	t.Helper()
	agents := newFakeAgents()
	rc := &RoutingChecker{Cluster: okChecker{}}
	c := newClusterWith(t, 1, func(_ string, cfg *Config) { cfg.Agents, cfg.Checker = agents, rc },
		func(_ string, s *Service) { rc.Store, rc.Ops = s.Store(), &Operations{Svc: s} })
	l := c.leader()
	ops := seed(t, l, 3)
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	return l, &Services{Ops: ops}, agents
}

func TestServiceCreateDeleteWithApproval(t *testing.T) {
	l, svcs, agents := serviceFixture(t)
	ctx := context.Background()
	in := containerInput(t, nil)

	// Deny by default: nobody and the operator preset (which holds only
	// service read) can neither preview nor create.
	for _, who := range []authz.Subject{nobody, operatorSubject} {
		if _, err := svcs.PreviewService(ctx, who, in); !errors.Is(err, api.ErrForbidden) {
			t.Fatalf("%s preview: %v", who.ID, err)
		}
		if _, err := svcs.CreateService(ctx, who, in); !errors.Is(err, api.ErrForbidden) {
			t.Fatalf("%s create: %v", who.ID, err)
		}
	}
	// A preview changes nothing.
	pv, err := svcs.PreviewService(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Executable || len(pv.Steps) != 3 || !strings.Contains(pv.Text, "create-service") || len(pv.Artifacts) != 1 {
		t.Fatalf("preview %+v", pv)
	}
	if n := len(agents.commands()); n != 0 {
		t.Fatalf("%d commands after a preview", n)
	}
	if list, _ := svcs.Services(ctx); len(list) != 0 {
		t.Fatalf("preview stored a service: %v", list)
	}
	for _, bad := range []api.ServiceInput{
		{Swarm: "nope", Name: "web", Type: "container", Spec: in.Spec},
		{Swarm: "sw1", Name: "web", Type: "galera", Spec: in.Spec},
		{Swarm: "sw1", Name: "web", Type: "container", Spec: json.RawMessage(`{"image":"nginx:latest"}`)},
		containerInput(t, func(c *service.ContainerSpec) { c.Placement.HostID = "h9" }),
	} {
		want := api.ErrInvalidInput
		if bad.Swarm == "nope" {
			want = api.ErrNotFound
		}
		if _, err := svcs.CreateService(ctx, adminSubject, bad); !errors.Is(err, want) {
			t.Errorf("%+v: %v, want %v", bad.Type, err, want)
		}
	}

	started, err := svcs.CreateService(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	if started.Operation.Kind != runner.OpServiceCreate || len(started.Approval.Digest) != 64 || started.Service == "" {
		t.Fatalf("%+v", started)
	}
	// Nothing runs before the approval; every command waits for it.
	waitOpStatus(t, l, started.Operation.ID, runner.StatusPaused)
	if n := len(agents.commands()); n != 0 {
		t.Fatalf("%d commands before the approval", n)
	}
	rec := opRecord(t, l, started.Operation.ID)
	if rec.Service != started.Service || rec.ApprovalDigest != started.Approval.Digest {
		t.Fatalf("%+v", rec)
	}
	var kinds []agent.Kind
	for _, st := range rec.Definition.Steps {
		kinds = append(kinds, st.Command)
		if !slices.ContainsFunc(st.Preconditions, func(c runner.Condition) bool { return c.Kind == "approved" }) {
			t.Errorf("step %s does not wait for the approval", st.ID)
		}
	}
	if !slices.Equal(kinds, []agent.Kind{agent.KindCreateVolume, agent.KindInitIdentity, agent.KindCreateService}) {
		t.Fatalf("steps %v", kinds)
	}
	if _, err := svcs.CreateService(ctx, adminSubject, in); !errors.Is(err, api.ErrClusterExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	// Approving needs create permission on the service resource.
	ctl := &Control{Ops: svcs.Ops, Swarms: &Swarms{}}
	if _, err := ctl.ApproveOperation(ctx, operatorSubject, started.Operation.ID, started.Approval.Digest); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("operator approved: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, started.Operation.ID, strings.Repeat("0", 64)); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, started.Operation.ID, started.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, started.Operation.ID, runner.StatusSucceeded)
	var created agent.ServiceSpec
	for _, c := range agents.commands() {
		if c.Cmd.Kind == agent.KindCreateService {
			if err := json.Unmarshal(c.Cmd.Args, &created); err != nil {
				t.Fatal(err)
			}
		}
	}
	if created.Name != "dbcm-svc-web" || created.Labels[service.LabelCluster] != started.Service || len(created.Mounts) != 1 {
		t.Fatalf("created %+v", created)
	}

	list, err := svcs.Services(ctx)
	if err != nil || len(list) != 1 || list[0].ID != started.Service || list[0].CreateOperation != started.Operation.ID {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := svcs.Service(ctx, "nope"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("%v", err)
	}

	// Delete: operator cannot; admin gets a plan and an approval digest.
	if _, err := svcs.DeleteService(ctx, operatorSubject, started.Service, false); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("operator delete: %v", err)
	}
	dp, err := svcs.PreviewServiceDelete(ctx, adminSubject, started.Service, true)
	if err != nil || len(dp.Steps) != 2 {
		t.Fatalf("%+v %v", dp, err)
	}
	before := len(agents.commands())
	del, err := svcs.DeleteService(ctx, adminSubject, started.Service, true)
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, del.Operation.ID, runner.StatusPaused)
	if len(agents.commands()) != before {
		t.Fatal("deletion ran before the approval")
	}
	if _, err := ctl.ApproveOperation(ctx, operatorSubject, del.Operation.ID, del.Approval.Digest); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("operator approved a deletion: %v", err)
	}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, del.Operation.ID, del.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, del.Operation.ID, runner.StatusSucceeded)
	waitFor(t, "service record deleted", func() bool {
		_, ok, _ := Get[ServiceRecord](l.Store(), CollServices, started.Service)
		return !ok
	})
	var removed []agent.Kind
	for _, c := range agents.commands() {
		if c.Cmd.OpID == del.Operation.ID {
			removed = append(removed, c.Cmd.Kind)
		}
	}
	if !slices.Equal(removed, []agent.Kind{agent.KindRemoveClusterService, agent.KindRemoveVolume}) {
		t.Fatalf("deletion commands %v", removed)
	}
	// The name is free again.
	if _, err := svcs.CreateService(ctx, adminSubject, in); err != nil {
		t.Fatalf("create after the deletion: %v", err)
	}
}

func TestServiceWithBlockersIsPreviewedButNotStarted(t *testing.T) {
	_, svcs, agents := serviceFixture(t)
	ctx := context.Background()
	// An NFS server needs privileges the agent cannot grant: always blocked.
	nfs, err := json.Marshal(service.NFSSpec{
		StorageBase: service.StorageBase{Image: "example/nfs:1", Placement: service.Placement{HostID: "h1"}},
		Exports:     []service.NFSExport{{Path: "/", Clients: []string{"10.0.0.0/24"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := api.ServiceInput{Swarm: "sw1", Name: "web", Type: "nfs", Spec: nfs}
	pv, err := svcs.PreviewService(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Executable || len(pv.Blockers) != 2 {
		t.Fatalf("%+v", pv)
	}
	if _, err := svcs.CreateService(ctx, adminSubject, in); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("blocked create: %v", err)
	}
	if list, _ := svcs.Services(ctx); len(list) != 0 || len(agents.commands()) != 0 {
		t.Fatal("a blocked plan left state or sent commands")
	}
}

func TestServiceRecordsHoldNoSecretValues(t *testing.T) {
	l, svcs, _ := serviceFixture(t)
	ctx := context.Background()
	in := containerInput(t, func(c *service.ContainerSpec) { c.Env = []service.EnvVar{{Name: "DB_PASSWORD", Value: "hunter2"}} })
	if _, err := svcs.CreateService(ctx, adminSubject, in); !errors.Is(err, api.ErrInvalidInput) {
		t.Fatalf("%v", err)
	}
	docs, _ := l.Store().RawList(CollServices)
	if len(docs) != 0 {
		t.Fatal("rejected spec was stored")
	}
}
