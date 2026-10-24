// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

func containerJSON(t *testing.T, c service.ContainerSpec) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var webCtx = service.Context{InstallationID: "inst-1", ServiceID: "svc-1", Name: "web"}

func TestContainerUpdatePlanStartFirstRollback(t *testing.T) {
	cur := containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.1", Replicas: 3})
	next := containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.2", Replicas: 3})
	p, err := service.UpdatePlan(service.Container{}, service.UpdateInput{Context: webCtx, Current: cur, Spec: next,
		Strategy: service.UpdateStrategy{Order: service.OrderStartFirst, Parallelism: 1, DelaySeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != service.PlanUpdate || !p.Executable() || !p.RequiresApproval {
		t.Fatalf("plan %+v", p)
	}
	kinds := []service.StepKind{service.StepUpdateService, service.StepAwaitUpdate, service.StepCheckHealth}
	for i, k := range kinds {
		if p.Steps[i].Kind != k {
			t.Fatalf("step %d is %s, want %s", i, p.Steps[i].Kind, k)
		}
	}
	u := p.Steps[0].Spec.UpdateConfig
	if u == nil || u.Order != "start-first" || u.FailureAction != "rollback" || u.Parallelism != 1 || u.Monitor != 30*time.Second || u.Delay != 5*time.Second {
		t.Fatalf("update config %+v", u)
	}
	if rb := p.Steps[0].Spec.RollbackConfig; rb == nil || rb.FailureAction != "pause" || rb.Order != "start-first" {
		t.Fatalf("rollback config %+v", rb)
	}
	if p.Rollback == nil || p.Rollback.Steps[0].Spec.Image != "nginx:1.27.1" {
		t.Fatalf("rollback plan %+v", p.Rollback)
	}

	op, err := runner.FromServicePlan(p, runner.ServiceBindings{OpID: "op1", InstallationID: "inst-1", VolumeBase: "/srv/dbcm",
		StepTimeout: time.Second, ServiceHost: "mgr1"})
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != runner.OpServiceUpdate || len(op.Steps) != 3 {
		t.Fatalf("op %+v", op)
	}
	for _, st := range op.Steps[:2] {
		if st.Target != "mgr1" {
			t.Errorf("step %s on %q", st.ID, st.Target)
		}
		if err := strictArgs(st.Command, st.Args); err != nil {
			t.Errorf("step %s: %v", st.ID, err)
		}
	}
	var ua agent.UpdateServiceArgs
	_ = json.Unmarshal(op.Steps[0].Args, &ua)
	if ua.ServiceName != "dbcm-svc-web" || ua.Spec.Image != "nginx:1.27.2" || ua.Spec.UpdateConfig == nil ||
		ua.Spec.UpdateConfig.Order != agent.UpdateStartFirst || ua.Spec.UpdateConfig.MonitorNS != int64(30*time.Second) {
		t.Fatalf("update args %+v", ua)
	}
	if err := agent.CheckUpdateConfig(ua.Spec.UpdateConfig, false); err != nil {
		t.Fatal(err)
	}
	if err := agent.CheckUpdateConfig(ua.Spec.RollbackConfig, true); err != nil {
		t.Fatal(err)
	}
	gate := op.Steps[2]
	if gate.Command != "" || len(gate.Preconditions) != 2 || gate.Preconditions[1].Kind != runner.CondServiceHealthy || gate.Preconditions[1].Target != "svc-1" {
		t.Fatalf("health gate %+v", gate)
	}
}

func TestUpdatePlanRefusals(t *testing.T) {
	vol := service.ContainerSpec{Image: "redis:7.2", Volumes: []service.Volume{{Name: "data", Target: "/data"}}, Placement: service.Placement{HostID: "h1"}}
	cur := containerJSON(t, vol)
	vol.Image = "redis:7.4"
	next := containerJSON(t, vol)
	in := service.UpdateInput{Context: webCtx, Current: cur, Spec: next, Strategy: service.UpdateStrategy{Order: service.OrderStartFirst}}
	if _, err := service.UpdatePlan(service.Container{}, in); !errors.Is(err, service.ErrInvalidSpec) {
		t.Fatalf("start-first with a local volume: %v", err)
	}
	in.Strategy.Order = ""
	p, err := service.UpdatePlan(service.Container{}, in)
	if err != nil || !p.Executable() {
		t.Fatalf("stop-first with a volume: %v %v", err, p.Blockers)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "unavailable while its task is replaced") {
		t.Errorf("warnings %v", p.Warnings)
	}

	// What update-service refuses to change is a blocker, not a silent drop.
	moved := vol
	moved.Placement.HostID = "h2"
	p, err = service.UpdatePlan(service.Container{}, service.UpdateInput{Context: webCtx, Current: cur, Spec: containerJSON(t, moved)})
	if err != nil || p.Executable() {
		t.Fatalf("placement change: %v, blockers %v", err, p.Blockers)
	}
	scaled := containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.2", Replicas: 4})
	p, _ = service.UpdatePlan(service.Container{}, service.UpdateInput{Context: webCtx,
		Current: containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.2", Replicas: 3}), Spec: scaled})
	if p.Executable() {
		t.Fatal("replica change executable through update-service")
	}
	if _, err := runner.FromServicePlan(p, runner.ServiceBindings{OpID: "o", InstallationID: "i", VolumeBase: "/srv", ServiceHost: "m"}); !errors.Is(err, service.ErrNotExecutable) {
		t.Fatalf("blocked update converted: %v", err)
	}

	// The canary field is accepted but never executed.
	p, err = service.UpdatePlan(service.Container{}, service.UpdateInput{Context: webCtx,
		Current: containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.1"}), Spec: containerJSON(t, service.ContainerSpec{Image: "nginx:1.27.2"}),
		Strategy: service.UpdateStrategy{Canary: &service.Canary{Replicas: 1, HoldSeconds: 60}}})
	if err != nil || p.Executable() || !strings.Contains(strings.Join(p.Blockers, "\n"), "canary") {
		t.Fatalf("canary: %v %v", err, p.Blockers)
	}

	for _, s := range []service.UpdateStrategy{
		{Order: "random"}, {Parallelism: -1}, {MonitorSeconds: 99999}, {MaxFailureRatio: 1.5}, {OnFailure: "continue"},
		{Canary: &service.Canary{}},
	} {
		if err := s.Validate(); !errors.Is(err, service.ErrInvalidSpec) {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestTraefikUpdateKeepsCreateBlockers(t *testing.T) {
	ty, _ := service.DefaultRegistry().Get(service.TypeTraefik)
	spec := json.RawMessage(`{"image":"traefik:v3.1.6","entryPoints":[{"name":"web","port":8080}]}`)
	if err := ty.Validate(spec); err != nil {
		t.Skipf("traefik fixture not valid for this version: %v", err)
	}
	p, err := service.UpdatePlan(ty, service.UpdateInput{Context: webCtx, Current: spec, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != service.TypeTraefik || !strings.Contains(strings.Join(p.Warnings, "\n"), "equals the running one") {
		t.Fatalf("plan %+v", p)
	}
}
