// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Updates of a running service (R6 "update strategy"). An update replaces
// the Swarm service spec through the agent's existing update-service
// command; the rolling behaviour is the Swarm UpdateConfig the plan sets on
// the spec, so Swarm itself replaces the tasks batch by batch, watches each
// new task for the monitor period and, with OnFailure "rollback", rolls the
// whole service back to the previous spec when a new task fails (a task
// that exits or whose container health check fails counts as failed). The
// plan then waits for the update to end (agent await-service: an update
// that Swarm rolled back or paused fails the step) and for the service to
// be healthy (a gate the manager's checker evaluates from observed task
// state). A failed gate stops the operation; the plan's Rollback is the
// prepared update back to the previous spec.
//
// Status: implemented and tested with fakes only.

// PlanUpdate is the kind of an update or rollback plan.
const PlanUpdate PlanKind = "update"

// Step kinds of update plans. update-service and await-service are agent
// command names (agentcompat_test.go).
const (
	StepUpdateService StepKind = "update-service"
	StepAwaitUpdate   StepKind = "await-service"
	// StepCheckHealth is a gate: the service must be healthy (every desired
	// task running) before the plan succeeds.
	StepCheckHealth StepKind = "check-health"
)

// Update orders and failure actions.
const (
	OrderStopFirst    = "stop-first"
	OrderStartFirst   = "start-first"
	OnFailureRollback = "rollback"
	OnFailurePause    = "pause"
)

// ErrUpdateUnsupported: the type has no update planner.
var ErrUpdateUnsupported = errors.New("service: the service type has no update plan")

// UpdateStrategy is the rolling-update model of a service.
type UpdateStrategy struct {
	// Order is "stop-first" (default: the old task stops before the new one
	// starts) or "start-first" (the new task starts first; refused for
	// services with local volumes, which two tasks must not share).
	Order string `json:"order,omitempty"`
	// Parallelism is the number of tasks replaced at once (default 1).
	Parallelism int `json:"parallelism,omitempty"`
	// DelaySeconds is the pause between batches.
	DelaySeconds int `json:"delaySeconds,omitempty"`
	// MonitorSeconds is how long Swarm watches each new task for a failure
	// (default 30).
	MonitorSeconds int `json:"monitorSeconds,omitempty"`
	// MaxFailureRatio is the tolerated share of failed tasks, 0..1
	// (default 0).
	MaxFailureRatio float64 `json:"maxFailureRatio,omitempty"`
	// OnFailure is "rollback" (default) or "pause".
	OnFailure string `json:"onFailure,omitempty"`
	// Canary is accepted so that a strategy can carry it, but no canary
	// executor exists yet: a plan with a canary has a blocker.
	Canary *Canary `json:"canary,omitempty"`
}

// Canary is a staged rollout: first Replicas tasks of the new version, held
// for HoldSeconds and judged healthy, then the rest. Not executable yet.
type Canary struct {
	Replicas    int `json:"replicas"`
	HoldSeconds int `json:"holdSeconds,omitempty"`
}

// Defaults of UpdateStrategy.
const (
	DefaultMonitorSeconds = 30
	maxStrategySeconds    = 3600
)

// withDefaults fills the defaults.
func (s UpdateStrategy) withDefaults() UpdateStrategy {
	if s.Order == "" {
		s.Order = OrderStopFirst
	}
	if s.Parallelism == 0 {
		s.Parallelism = 1
	}
	if s.MonitorSeconds == 0 {
		s.MonitorSeconds = DefaultMonitorSeconds
	}
	if s.OnFailure == "" {
		s.OnFailure = OnFailureRollback
	}
	return s
}

// Validate checks the strategy.
func (s UpdateStrategy) Validate() error {
	var p problems
	switch s.Order {
	case "", OrderStopFirst, OrderStartFirst:
	default:
		p.addf("order %q (want stop-first or start-first)", s.Order)
	}
	if s.Parallelism < 0 || s.Parallelism > 50 {
		p.addf("parallelism %d out of range 1..50", s.Parallelism)
	}
	if s.DelaySeconds < 0 || s.DelaySeconds > maxStrategySeconds {
		p.addf("delaySeconds %d out of range 0..%d", s.DelaySeconds, maxStrategySeconds)
	}
	if s.MonitorSeconds < 0 || s.MonitorSeconds > maxStrategySeconds {
		p.addf("monitorSeconds %d out of range 0..%d", s.MonitorSeconds, maxStrategySeconds)
	}
	if s.MaxFailureRatio < 0 || s.MaxFailureRatio > 1 || s.MaxFailureRatio != s.MaxFailureRatio {
		p.addf("maxFailureRatio %v out of range 0..1", s.MaxFailureRatio)
	}
	switch s.OnFailure {
	case "", OnFailureRollback, OnFailurePause:
	default:
		p.addf("onFailure %q (want rollback or pause)", s.OnFailure)
	}
	if c := s.Canary; c != nil && (c.Replicas < 1 || c.HoldSeconds < 0 || c.HoldSeconds > maxStrategySeconds) {
		p.addf("canary needs replicas >= 1 and holdSeconds 0..%d", maxStrategySeconds)
	}
	return p.err()
}

// swarmConfigs returns the Swarm update and rollback configs of the
// strategy. The rollback uses the same order and parallelism and pauses on
// failure (Swarm cannot roll back a rollback).
func (s UpdateStrategy) swarmConfigs() (update, rollback *SwarmUpdateConfig) {
	s = s.withDefaults()
	update = &SwarmUpdateConfig{
		Parallelism: uint64(s.Parallelism), Delay: time.Duration(s.DelaySeconds) * time.Second,
		FailureAction: s.OnFailure, Monitor: time.Duration(s.MonitorSeconds) * time.Second,
		MaxFailureRatio: s.MaxFailureRatio, Order: s.Order,
	}
	rollback = &SwarmUpdateConfig{
		Parallelism: uint64(s.Parallelism), FailureAction: OnFailurePause,
		Monitor: time.Duration(s.MonitorSeconds) * time.Second, Order: s.Order,
	}
	return update, rollback
}

// UpdateInput is the input of UpdatePlan.
type UpdateInput struct {
	Context
	// Current is the stored (running) spec, Spec the desired one.
	Current  json.RawMessage
	Spec     json.RawMessage
	Strategy UpdateStrategy
	Hosts    []Host
}

// Updater is implemented by service types that can update a running
// service in place.
type Updater interface {
	UpdatePlan(in UpdateInput) (Plan, error)
}

// UpdatePlan returns the update plan of a type, or ErrUpdateUnsupported.
func UpdatePlan(t Type, in UpdateInput) (Plan, error) {
	u, ok := t.(Updater)
	if !ok {
		return Plan{}, fmt.Errorf("%w: %q", ErrUpdateUnsupported, t.Name())
	}
	return u.UpdatePlan(in)
}

// UpdatePlan implements Updater.
func (c Container) UpdatePlan(in UpdateInput) (Plan, error) { return planUpdate(c, in) }

// UpdatePlan implements Updater. Traefik's dynamic configuration has no
// delivery command, so the blockers of its create plan apply.
func (t Traefik) UpdatePlan(in UpdateInput) (Plan, error) { return planUpdate(t, in) }

// createdSpec returns the Swarm spec of a create plan.
func createdSpec(p Plan) (ServiceSpec, bool) {
	for _, s := range p.Steps {
		if s.Kind == StepCreateService && s.Spec != nil {
			return *s.Spec, true
		}
	}
	return ServiceSpec{}, false
}

// planUpdate plans an update of a type whose create plan has exactly one
// create-service step. The current and desired specs are planned the same
// way, so the update changes exactly what a new creation would differ in.
func planUpdate(t Type, in UpdateInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	if err := in.Strategy.Validate(); err != nil {
		return Plan{}, err
	}
	cur, err := t.Plan(PlanInput{Context: in.Context, Spec: in.Current})
	if err != nil {
		return Plan{}, fmt.Errorf("current spec: %w", err)
	}
	next, err := t.Plan(PlanInput{Context: in.Context, Spec: in.Spec, Hosts: in.Hosts})
	if err != nil {
		return Plan{}, err
	}
	from, ok1 := createdSpec(cur)
	to, ok2 := createdSpec(next)
	if !ok1 || !ok2 {
		return Plan{}, fmt.Errorf("%w: %q plans no service", ErrUpdateUnsupported, t.Name())
	}
	strat := in.Strategy.withDefaults()
	if strat.Order == OrderStartFirst && len(to.Mounts) > 0 {
		return Plan{}, fmt.Errorf("%w: start-first would run two tasks on the same local volume; use stop-first", ErrInvalidSpec)
	}
	p := updateSteps(in.Context, t.Name(), from, to, strat)
	p.Blockers = append(p.Blockers, next.Blockers...)
	p.Warnings = append(p.Warnings, next.Warnings...)
	p.Artifacts = next.Artifacts
	p.Blockers = append(p.Blockers, immutableChanges(from, to)...)
	if strat.Canary != nil {
		p.Blockers = append(p.Blockers, "a canary rollout cannot be executed yet: the strategy field is accepted, but no canary executor exists")
	}
	if equalSpecs(from, to) {
		p.Warnings = append(p.Warnings, "the desired spec equals the running one; the update only applies the update strategy")
	}
	if to.Replicas == 1 && strat.Order == OrderStopFirst {
		p.Warnings = append(p.Warnings, "one replica with stop-first: the service is unavailable while its task is replaced")
	}
	rb := updateSteps(in.Context, t.Name(), to, from, strat)
	rb.Warnings = append(rb.Warnings, "rollback plan: restores the previous spec with the same strategy; it is started separately and needs its own approval")
	p.Rollback = &rb
	return p, nil
}

// updateSteps are update-service, await-service and the health gate.
func updateSteps(ctx Context, typ string, from, to ServiceSpec, strat UpdateStrategy) Plan {
	upd, rb := strat.swarmConfigs()
	spec := to
	spec.UpdateConfig, spec.RollbackConfig = upd, rb
	p := Plan{Kind: PlanUpdate, Type: typ, ServiceID: ctx.ServiceID, Name: ctx.Name, RequiresApproval: true}
	how := fmt.Sprintf("%s, %d task(s) at a time, each watched for %ds", strat.Order, strat.Parallelism, strat.MonitorSeconds)
	if strat.OnFailure == OnFailureRollback {
		how += "; Swarm rolls back to the previous spec when a new task fails"
	} else {
		how += "; Swarm pauses the update when a new task fails"
	}
	p.Steps = []Step{
		{ID: "01-update-service-" + ctx.Name, Kind: StepUpdateService, Spec: &spec, ServiceName: spec.Name, ServiceID: ctx.ServiceID,
			Note: fmt.Sprintf("update the Swarm service %s from image %s to %s (%s)", spec.Name, from.Image, to.Image, how)},
		{ID: "02-await-service-" + ctx.Name, Kind: StepAwaitUpdate, ServiceName: spec.Name, ServiceID: ctx.ServiceID,
			After: []string{"01-update-service-" + ctx.Name},
			Note:  "wait until Swarm has finished the update; a paused or rolled-back update fails this step"},
		{ID: "03-check-health-" + ctx.Name, Kind: StepCheckHealth, ServiceName: spec.Name, ServiceID: ctx.ServiceID,
			After: []string{"02-await-service-" + ctx.Name},
			Note:  "wait until every desired task of the service runs (observed Swarm task state); otherwise the operation fails and the rollback plan is offered"},
	}
	return p
}

// immutableChanges lists what the agent's update-service refuses to change.
func immutableChanges(from, to ServiceSpec) []string {
	var out []string
	if from.Replicas != to.Replicas {
		out = append(out, fmt.Sprintf("replicas change from %d to %d: the agent's update-service keeps the replica count", from.Replicas, to.Replicas))
	}
	if !slices.Equal(from.Constraints, to.Constraints) {
		out = append(out, "the placement changes: the agent's update-service keeps the placement constraints (delete and recreate the service)")
	}
	if !slices.Equal(from.Mounts, to.Mounts) {
		out = append(out, "the volumes change: the agent's update-service keeps the mounts (delete and recreate the service)")
	}
	return out
}

func equalSpecs(a, b ServiceSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
