// SPDX-License-Identifier: AGPL-3.0-only

package manager

// Fakes only: fake agents, a fake Swarm connector, a fake registry
// resolver and the in-process store. No registry or Swarm is contacted.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

type iuResolver struct {
	tags    []string
	digests map[string]string
}

func (r iuResolver) Tags(context.Context, imageupdate.Ref) ([]string, error) { return r.tags, nil }
func (r iuResolver) Digest(_ context.Context, ref imageupdate.Ref) (string, error) {
	if d, ok := r.digests[ref.Tag]; ok {
		return d, nil
	}
	return "", errors.New("unknown tag")
}

const (
	iuDigestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	iuDigestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func newImageUpdateSvc(t *testing.T, svcs *Services, res imageupdate.Resolver) (*imageupdate.Service, *ImageUpdateAdapter) {
	t.Helper()
	ad := &ImageUpdateAdapter{Svc: svcs}
	return &imageupdate.Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Source: ad, Driver: ad, Resolver: res, Installation: "inst-1",
		Term:      func() (uint64, error) { return 1, nil },
		PollEvery: 20 * time.Millisecond}, ad
}

func TestImageUpdateAdapterListsOnlyOwnedServicesAndUpdatesThroughTheOperation(t *testing.T) {
	l, svcs, _, conn := updateFixture(t)
	ctx := context.Background()
	in := containerInput(t, func(c *service.ContainerSpec) { c.Volumes, c.Replicas = nil, 1; c.Placement.HostID = "" })
	started, err := svcs.CreateService(ctx, adminSubject, in)
	if err != nil {
		t.Fatal(err)
	}
	ctl := &Control{Ops: svcs.Ops, Swarms: &Swarms{}}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, started.Operation.ID, started.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, started.Operation.ID, runner.StatusSucceeded)
	id := started.Service
	conn.set(tasks(id, 1, 1, "completed"))

	iu, ad := newImageUpdateSvc(t, svcs, iuResolver{tags: []string{"1.27.2", "1.27.3"}, digests: map[string]string{"1.27.3": iuDigestB}})
	iu.Installation = ""
	if views, _ := iu.Services(ctx); len(views) != 0 {
		t.Fatal("services listed without an installation ID")
	}
	inst, err := ad.installation()
	if err != nil {
		t.Fatal(err)
	}
	iu.Installation = inst
	views, err := iu.Services(ctx)
	if err != nil || len(views) != 1 || views[0].Service.ID != id || views[0].Service.Image != "nginx:1.27.2" || views[0].Scope != imageupdate.ScopeOwned {
		t.Fatalf("%+v %v", views, err)
	}

	if _, err := iu.SetPolicy(ctx, "admin", imageupdate.Policy{ServiceID: id, Mode: imageupdate.ModeNotify, Track: imageupdate.TrackPatch,
		Strategy: imageupdate.Strategy{Order: "start-first"}}); err != nil {
		t.Fatal(err)
	}
	res, err := iu.Check(ctx, id, imageupdate.CheckOptions{Actor: "admin"})
	if err != nil || res.Plan == nil || res.Plan.To.Ref != "nginx:1.27.3@"+iuDigestB {
		t.Fatalf("%+v %v", res, err)
	}
	// A user without the update permission cannot approve the service
	// operation: the existing authorization stays in force.
	if _, err := iu.Approve(ctx, "admin", res.Plan.ID, res.Plan.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := iu.Apply(ctx, "nobody", res.Plan.ID); err == nil {
		t.Fatal("applied by a subject without a service update permission")
	}
	got, _ := iu.Plan(res.Plan.ID)
	if got.State != imageupdate.PlanFailed && got.State != imageupdate.PlanApproved {
		t.Fatalf("%+v", got)
	}
}

func TestImageUpdateApplyRunsTheServiceUpdateAndRecordsRuntime(t *testing.T) {
	l, svcs, agents, conn := updateFixture(t)
	agents.runtime = func(agent.Command) agentrt.Runtime { return agentrt.Container }
	ctx := context.Background()
	in := containerInput(t, func(c *service.ContainerSpec) { c.Volumes, c.Replicas = nil, 1; c.Placement.HostID = "" })
	started, _ := svcs.CreateService(ctx, adminSubject, in)
	ctl := &Control{Ops: svcs.Ops, Swarms: &Swarms{}}
	if _, err := ctl.ApproveOperation(ctx, adminSubject, started.Operation.ID, started.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, started.Operation.ID, runner.StatusSucceeded)
	id := started.Service
	conn.set(tasks(id, 1, 1, "completed"))

	iu, ad := newImageUpdateSvc(t, svcs, iuResolver{tags: []string{"1.27.2", "1.27.3"}, digests: map[string]string{"1.27.3": iuDigestB}})
	iu.Installation, _ = ad.installation()
	if _, err := iu.SetPolicy(ctx, "admin", imageupdate.Policy{ServiceID: id, Mode: imageupdate.ModeNotify, Track: imageupdate.TrackPatch,
		Strategy: imageupdate.Strategy{Order: "start-first", MonitorSeconds: 20}}); err != nil {
		t.Fatal(err)
	}
	res, err := iu.Check(ctx, id, imageupdate.CheckOptions{Actor: "admin"})
	if err != nil || res.Plan == nil {
		t.Fatalf("%+v %v", res, err)
	}
	p, err := iu.Approve(ctx, "admin", res.Plan.ID, res.Plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// The apply authorizes as the human; the fixture's administrator is
	// bound to the administrator role.
	p, err = iu.Apply(ctx, adminSubject.ID, p.ID)
	if err != nil {
		t.Fatalf("%v %+v", err, p)
	}
	if p.State != imageupdate.PlanSucceeded || p.ExecutedBy != agentrt.Container || p.OperationID == "" {
		t.Fatalf("%+v", p)
	}
	var ua agent.UpdateServiceArgs
	for _, c := range agents.commands() {
		if c.Cmd.Kind == agent.KindUpdateService {
			_ = json.Unmarshal(c.Cmd.Args, &ua)
		}
	}
	if ua.Spec.Image != "nginx:1.27.3@"+iuDigestB || ua.Spec.UpdateConfig == nil || ua.Spec.UpdateConfig.Order != agent.UpdateStartFirst ||
		ua.Spec.UpdateConfig.FailureAction != agent.FailureActionRollback || ua.Spec.RollbackConfig == nil {
		t.Fatalf("update args %+v", ua.Spec)
	}
	rec, _, _ := Get[ServiceRecord](l.Store(), CollServices, id)
	if !strings.Contains(string(rec.Value.Spec), "nginx:1.27.3@"+iuDigestB) || !strings.Contains(string(rec.Value.PreviousSpec), "nginx:1.27.2") {
		t.Fatalf("recorded spec %s previous %s", rec.Value.Spec, rec.Value.PreviousSpec)
	}
	if st, _ := iu.State(id); st.Baseline == nil || st.Baseline.Digest != iuDigestB {
		t.Fatalf("%+v", st)
	}
}

func TestImageUpdateStatusMapping(t *testing.T) {
	step := func(id string, cmd agent.Kind, res *agent.Result) runner.StepView {
		return runner.StepView{ID: id, Command: cmd, Target: "h1", LastResult: res, Runtime: agentrt.Container}
	}
	cases := []struct {
		name string
		view *runner.View
		want imageupdate.OpState
	}{
		{"no view yet", nil, imageupdate.OpRunning},
		{"running", &runner.View{Status: runner.StatusRunning}, imageupdate.OpRunning},
		{"paused", &runner.View{Status: runner.StatusPaused, PausedStep: "03-check-health-web"}, imageupdate.OpRunning},
		{"succeeded", &runner.View{Status: runner.StatusSucceeded}, imageupdate.OpSucceeded},
		{"failed", &runner.View{Status: runner.StatusFailed, Reason: "step failed"}, imageupdate.OpFailed},
		{"rolled back by swarm", &runner.View{Status: runner.StatusFailed, Steps: []runner.StepView{
			step("01", agent.KindUpdateService, &agent.Result{Status: agent.StatusSucceeded}),
			step("02", agent.KindAwaitService, &agent.Result{Status: agent.StatusFailed, Code: agent.CodeUpdateFailed, Message: `service "x" update state "rollback_completed"`})}}, imageupdate.OpRolledBack},
		{"paused by swarm", &runner.View{Status: runner.StatusFailed, Steps: []runner.StepView{
			step("02", agent.KindAwaitService, &agent.Result{Status: agent.StatusFailed, Code: agent.CodeUpdateFailed, Message: `service "x" update state "paused"`})}}, imageupdate.OpFailed},
		{"aborted", &runner.View{Status: runner.StatusAborted}, imageupdate.OpFailed},
	}
	for _, c := range cases {
		got := opStatusOf(OperationRecord{View: c.view})
		if got.State != c.want {
			t.Errorf("%s: %s, want %s", c.name, got.State, c.want)
		}
	}
	st := opStatusOf(OperationRecord{View: &runner.View{Status: runner.StatusSucceeded, Steps: []runner.StepView{step("01", agent.KindUpdateService, nil)}}})
	if st.Runtime != agentrt.Container {
		t.Fatalf("runtime %q", st.Runtime)
	}
}

func TestImageUpdateBusyMapsToBusy(t *testing.T) {
	if !errors.Is(mapImageUpdateError(api.ErrBusy), imageupdate.ErrBusy) {
		t.Fatal("api busy")
	}
	if !errors.Is(mapImageUpdateError(runnerBusy()), imageupdate.ErrBusy) {
		t.Fatal("runner busy")
	}
	if err := errors.New("other"); mapImageUpdateError(err) != err {
		t.Fatal("other errors must pass through")
	}
}

func runnerBusy() error { return runner.ErrBusy }
