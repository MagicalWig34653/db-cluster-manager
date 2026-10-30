// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

func TestClassify(t *testing.T) {
	for _, n := range []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin", "docker.io", "moby-engine", "moby-cli", "moby-containerd"} {
		if Classify(n) != ClassContainerRuntime {
			t.Errorf("%s not container-runtime", n)
		}
	}
	for _, n := range []string{"openssl", "docker-doc", "mobyfoo", "containerd", "curl"} {
		if Classify(n) != ClassOS {
			t.Errorf("%s classified %s", n, Classify(n))
		}
	}
}

func TestPolicyContainerRuntimeDefaultsToReportOnly(t *testing.T) {
	p := Policy{Mode: ModeApprovalRequired}.Normalized()
	if p.ContainerRuntime != ModeReportOnly {
		t.Fatal(p.ContainerRuntime)
	}
	if err := (Policy{ContainerRuntime: "auto"}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestEvaluateEngine(t *testing.T) {
	ok := EngineState{Version: "29.1.0", DockerInfoOK: true, SwarmRole: "worker", NodeState: "ready", Availability: "drain", TasksKnown: true, LiveRestore: No}
	if r := EvaluateEngine(ok, nil); !r.Allowed || !strings.Contains(r.RestartNote, "all containers") {
		t.Fatalf("%+v", r)
	}
	busy := ok
	busy.RunningTasks = []string{"web.1.abc", "db.1.def"}
	r := EvaluateEngine(busy, []string{"web.1.abc"})
	if r.Allowed || !strings.Contains(failedDetail(r, "tasks"), "db.1.def") || strings.Contains(failedDetail(r, "tasks"), "web.1.abc") {
		t.Fatalf("%+v", r)
	}
	if !EvaluateEngine(busy, []string{"web.1.abc", "db.1.def"}).Allowed {
		t.Fatal("acknowledged tasks still refused")
	}
	unknown := ok
	unknown.TasksKnown = false
	if EvaluateEngine(unknown, nil).Allowed {
		t.Fatal("unknown task list allowed")
	}
	mgr := ok
	mgr.SwarmRole, mgr.QuorumSafe = "manager", Unknown
	if EvaluateEngine(mgr, nil).Allowed {
		t.Fatal("manager with unknown quorum allowed")
	}
	mgr.QuorumSafe = Yes
	if !EvaluateEngine(mgr, nil).Allowed {
		t.Fatal("safe manager refused")
	}
	notReady := ok
	notReady.NodeState = "down"
	if EvaluateEngine(notReady, nil).Allowed {
		t.Fatal("down node allowed")
	}
	lr := ok
	lr.LiveRestore = Yes
	if !strings.Contains(EvaluateEngine(lr, nil).RestartNote, "live-restore") {
		t.Fatal("live-restore not reported")
	}
}

func failedDetail(r EngineGuardResult, name string) string {
	for _, c := range r.Checks {
		if c.Name == name && !c.OK {
			return c.Detail
		}
	}
	return ""
}

type fakeProbe struct {
	mu     sync.Mutex
	states []EngineState // consumed in order; the last one repeats
	err    error
	calls  int
}

func (p *fakeProbe) Probe(context.Context, string) (EngineState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return EngineState{}, p.err
	}
	s := p.states[0]
	if len(p.states) > 1 {
		p.states = p.states[1:]
	}
	return s, nil
}

func engineFixture(t *testing.T, pol *Policy) (*fixture, *fakeProbe) {
	t.Helper()
	f := newFixture(t)
	f.host.pending["docker-ce"] = "5:29.2.0-1"
	f.host.pending["containerd.io"] = "1.7.30-1"
	f.host.report.Updates = append(f.host.report.Updates,
		Update{Name: "docker-ce", Installed: "5:29.1.0-1", Available: "5:29.2.0-1"},
		Update{Name: "containerd.io", Installed: "1.7.29-1", Available: "1.7.30-1"})
	pr := &fakeProbe{states: []EngineState{{Version: "29.1.0", DockerInfoOK: true, SwarmRole: "worker", NodeState: "ready", Availability: "drain", TasksKnown: true, LiveRestore: No}}}
	f.svc.Engine = pr
	f.svc.Sleep = func(time.Duration) {}
	f.setup(t, pol)
	return f, pr
}

var runtimeApproval = &Policy{Mode: ModeApprovalRequired, ContainerRuntime: ModeApprovalRequired}

func TestEngineUpdatesAreClassifiedAndNotInDefaultSelection(t *testing.T) {
	f, _ := engineFixture(t, runtimeApproval)
	rep, _ := f.svc.Report("h1")
	for _, u := range rep.Updates {
		want := ClassOS
		if u.Name == "docker-ce" || u.Name == "containerd.io" {
			want = ClassContainerRuntime
		}
		if u.Class != want {
			t.Errorf("%s class %q", u.Name, u.Class)
		}
	}
	p, err := f.svc.Propose(context.Background(), "alice", "h1", ProposeInput{})
	if err != nil {
		t.Fatal(err)
	}
	if p.ContainerRuntime || strings.Join(p.Packages, ",") != "curl,openssl,tzdata" {
		t.Fatalf("all-packages plan includes engine: %+v", p)
	}
}

func TestEnginePlanNeedsOwnPlanAndExplicitNames(t *testing.T) {
	f, _ := engineFixture(t, runtimeApproval)
	_, err := f.svc.Propose(context.Background(), "alice", "h1", ProposeInput{Packages: []string{"openssl", "docker-ce"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	p, err := f.svc.Propose(context.Background(), "alice", "h1", ProposeInput{Packages: []string{"docker-ce", "containerd.io"}, AcknowledgeTasks: []string{"web.1.abc"}})
	if err != nil || !p.ContainerRuntime || p.EngineGuard == nil || !p.EngineGuard.Allowed || p.EngineGuard.RestartNote == "" {
		t.Fatalf("%+v %v", p, err)
	}
	// The acknowledgment is part of the digest.
	q := p
	q.AcknowledgeTasks = []string{"other.1.x"}
	if q.computeDigest() == p.Digest {
		t.Fatal("acknowledged tasks not covered by the digest")
	}
}

func TestEnginePlanReportOnlyByDefault(t *testing.T) {
	f, pr := engineFixture(t, approval) // OS patches approvable, runtime not opted in
	ctx := context.Background()
	p, err := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"docker-ce"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, "bob", p.ID, p.Digest); !errors.Is(err, ErrRuntimeReportOnly) {
		t.Fatal("engine plan approved under default policy:", err)
	}
	if f.host.count(CmdApply) != 0 || pr.calls == 0 {
		t.Fatal("unexpected agent apply / no preview probe")
	}
}

func TestEngineApplyRefusedWhenTasksRunning(t *testing.T) {
	f, pr := engineFixture(t, runtimeApproval)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"docker-ce"}})
	if _, err := f.svc.Approve(ctx, "bob", p.ID, p.Digest); err != nil {
		t.Fatal(err)
	}
	pr.states = []EngineState{{Version: "29.1.0", DockerInfoOK: true, SwarmRole: "worker", NodeState: "ready", Availability: "active", TasksKnown: true, RunningTasks: []string{"web.1.abc"}}}
	got, err := f.svc.Apply(ctx, p.ID)
	if !errors.Is(err, ErrEngineGuard) || !strings.Contains(err.Error(), "web.1.abc") {
		t.Fatal(err)
	}
	if got.State != PlanApproved || f.host.count(CmdApply) != 0 {
		t.Fatalf("refused apply changed state or reached the host: %s", got.State)
	}
}

func TestEngineApplyGaleraGuardStillApplies(t *testing.T) {
	f, _ := engineFixture(t, runtimeApproval)
	f.svc.Guard = denyGuard{err: ErrGaleraBlocked}
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"docker-ce"}})
	f.svc.Approve(ctx, "bob", p.ID, p.Digest)
	if _, err := f.svc.Apply(ctx, p.ID); !errors.Is(err, ErrGaleraBlocked) {
		t.Fatal(err)
	}
}

func TestEngineApplyVerifiesAndRecordsVersions(t *testing.T) {
	f, pr := engineFixture(t, runtimeApproval)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"docker-ce"}})
	f.svc.Approve(ctx, "bob", p.ID, p.Digest)
	healthy := EngineState{Version: "29.2.0", DockerInfoOK: true, SwarmRole: "worker", NodeState: "ready", Availability: "drain", TasksKnown: true, TasksConverged: Yes, LiveRestore: No}
	// 1: guard at apply; 2: first verification (daemon still restarting); 3: healthy.
	pr.states = []EngineState{pr.states[0], {Version: "29.1.0", DockerInfoOK: false}, healthy}
	done, err := f.svc.Apply(ctx, p.ID)
	if err != nil || done.State != PlanSucceeded {
		t.Fatal(done, err)
	}
	if done.EngineVersionBefore != "29.1.0" || done.EngineVersionAfter != "29.2.0" {
		t.Fatalf("%+v", done)
	}
	op, _ := f.svc.Journal.Get(done.OpID)
	if len(op.Steps) != 3 || op.Steps[2].ID != "engine-verify" || op.Steps[2].State != opjournal.Succeeded {
		t.Fatalf("%+v", op.Steps)
	}
	hist, _ := f.svc.History("h1")
	if !strings.Contains(hist[0].Result, "29.1.0 -> 29.2.0") {
		t.Fatalf("%+v", hist[0])
	}
}

func TestEngineApplyFailsWhenHealthNotRestored(t *testing.T) {
	f, pr := engineFixture(t, runtimeApproval)
	ctx := context.Background()
	p, _ := f.svc.Propose(ctx, "alice", "h1", ProposeInput{Packages: []string{"docker-ce"}})
	f.svc.Approve(ctx, "bob", p.ID, p.Digest)
	bad := EngineState{Version: "29.2.0", DockerInfoOK: true, NodeState: "ready", TasksKnown: true, TasksConverged: No}
	pr.states = []EngineState{pr.states[0], bad}
	done, err := f.svc.Apply(ctx, p.ID)
	if err == nil || done.State != PlanFailed || !strings.Contains(done.Error, "reconverge") {
		t.Fatal(done, err)
	}
	if done.EngineVersionAfter != "29.2.0" {
		t.Fatal("post version not recorded on failure")
	}
}
