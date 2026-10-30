// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Class is the package class of an update. Container-runtime packages are
// the Docker engine and its siblings (owner decision 2026-10-30, ADR 0019
// and ADR 0023): they are patched by this module like any OS package, but
// they are report-only by default and need an engine-specific guard,
// because restarting the daemon affects every container on the host.
type Class string

// Package classes.
const (
	ClassOS               Class = "os"
	ClassContainerRuntime Class = "container-runtime"
)

var runtimePackages = []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin", "docker.io"}

// Classify returns the class of a package name.
func Classify(name string) Class {
	if slices.Contains(runtimePackages, name) || strings.HasPrefix(name, "moby-") {
		return ClassContainerRuntime
	}
	return ClassOS
}

// Errors of the engine guard.
var (
	ErrRuntimeReportOnly = errors.New("patch: container-runtime packages are report-only under this policy; applying is not allowed")
	ErrEngineGuard       = errors.New("patch: blocked: engine guard refused the update")
)

// EngineState is what the engine probe reports about a host. The probe is
// read-only: it asks the Swarm inventory and the host's Docker daemon.
type EngineState struct {
	Version      string `json:"version,omitempty"`
	DockerInfoOK bool   `json:"dockerInfoOk"`
	SwarmRole    string `json:"swarmRole,omitempty"`    // manager, worker or empty (not in a Swarm)
	NodeState    string `json:"nodeState,omitempty"`    // Swarm node status, "ready"
	Availability string `json:"availability,omitempty"` // active, pause, drain
	// RunningTasks names the Swarm tasks running on the host; TasksKnown
	// says whether the list could be determined (unknown fails closed).
	RunningTasks []string `json:"runningTasks,omitempty"`
	TasksKnown   bool     `json:"tasksKnown"`
	// QuorumSafe is the ADR 0019 manager quorum math: a majority of
	// managers stays ready and active outside this host.
	QuorumSafe Tristate `json:"quorumSafe,omitempty"`
	// LiveRestore is the daemon's live-restore setting.
	LiveRestore Tristate `json:"liveRestore,omitempty"`
	// TasksConverged says every desired Swarm task is running again.
	TasksConverged Tristate `json:"tasksConverged,omitempty"`
}

// EngineProbe reads the engine state of a host without changing it.
type EngineProbe interface {
	Probe(ctx context.Context, hostID string) (EngineState, error)
}

// EngineCheck is one guard check.
type EngineCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// EngineGuardResult is the outcome of the engine guard. Galera membership
// is not part of it: that stays with the Guard (host-maintenance
// coordination).
type EngineGuardResult struct {
	Allowed bool          `json:"allowed"`
	Checks  []EngineCheck `json:"checks"`
	// RestartNote says what a daemon restart does on this host.
	RestartNote string `json:"restartNote"`
	Version     string `json:"version,omitempty"`
}

// Failed lists the failed checks as one line.
func (r EngineGuardResult) Failed() string {
	var out []string
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return strings.Join(out, "; ")
}

// EvaluateEngine applies the engine guard rules to a probed state. ack names
// the running tasks the operator knowingly accepts to interrupt.
func EvaluateEngine(st EngineState, ack []string) EngineGuardResult {
	r := EngineGuardResult{Version: st.Version}
	add := func(name string, ok bool, detail string) {
		r.Checks = append(r.Checks, EngineCheck{Name: name, OK: ok, Detail: detail})
	}
	add("docker-info", st.DockerInfoOK, ternary(st.DockerInfoOK, "docker info answers", "docker info did not answer"))
	add("node-ready", st.NodeState == "ready", "node state is "+orUnknown(st.NodeState))
	switch {
	case !st.TasksKnown:
		add("tasks", false, "the running tasks of the host could not be determined")
	default:
		var open []string
		for _, t := range st.RunningTasks {
			if !slices.Contains(ack, t) {
				open = append(open, t)
			}
		}
		if len(open) == 0 {
			add("tasks", true, fmt.Sprintf("no unacknowledged running tasks (availability %s)", orUnknown(st.Availability)))
		} else {
			add("tasks", false, "running tasks not acknowledged: "+strings.Join(open, ", ")+"; drain the node first or acknowledge them")
		}
	}
	if st.SwarmRole == "manager" {
		add("manager-quorum", st.QuorumSafe == Yes, "Swarm manager quorum outside this host: "+string(orTri(st.QuorumSafe)))
	}
	switch st.LiveRestore {
	case Yes:
		add("live-restore", true, "live-restore is enabled")
		r.RestartNote = "A daemon restart affects the engine on this host; live-restore is enabled, so running containers should keep running, which is not guaranteed for every change."
	default:
		add("live-restore", true, "live-restore is "+string(orTri(st.LiveRestore)))
		r.RestartNote = "A daemon restart affects all containers on this host: they stop and restart (live-restore is " + string(orTri(st.LiveRestore)) + ")."
	}
	r.Allowed = true
	for _, c := range r.Checks {
		r.Allowed = r.Allowed && c.OK
	}
	return r
}

// EvaluateEngineHealth checks the post-update state. It returns the failed
// aspects (empty when healthy).
func EvaluateEngineHealth(st EngineState) string {
	var bad []string
	if !st.DockerInfoOK {
		bad = append(bad, "docker info does not answer")
	}
	if st.NodeState != "ready" {
		bad = append(bad, "node is "+orUnknown(st.NodeState))
	}
	if st.TasksConverged != Yes {
		bad = append(bad, "tasks have not reconverged")
	}
	return strings.Join(bad, "; ")
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orTri(t Tristate) Tristate {
	if t == "" {
		return Unknown
	}
	return t
}

const (
	engineVerifyAttempts = 5
	engineVerifyWait     = 10 * time.Second
)

// engineGuard probes the host and evaluates the guard. A probe failure is a
// refusal, never a pass.
func (s *Service) engineGuard(ctx context.Context, hostID string, ack []string) (EngineGuardResult, EngineState) {
	if s.Engine == nil {
		return EngineGuardResult{Checks: []EngineCheck{{Name: "probe", Detail: "no engine probe configured"}},
			RestartNote: "A daemon restart affects all containers on this host."}, EngineState{}
	}
	st, err := s.Engine.Probe(ctx, hostID)
	if err != nil {
		return EngineGuardResult{Checks: []EngineCheck{{Name: "probe", Detail: truncate(err.Error(), 200)}},
			RestartNote: "A daemon restart affects all containers on this host."}, EngineState{}
	}
	return EvaluateEngine(st, ack), st
}

// verifyEngine polls the probe until the engine is healthy again or the
// attempts run out, because the daemon restarts during the update.
func (s *Service) verifyEngine(ctx context.Context, hostID string) (EngineState, string) {
	var st EngineState
	last := "no engine probe configured"
	for i := 0; i < engineVerifyAttempts && s.Engine != nil; i++ {
		if i > 0 {
			if s.Sleep != nil {
				s.Sleep(engineVerifyWait)
			} else {
				time.Sleep(engineVerifyWait)
			}
		}
		got, err := s.Engine.Probe(ctx, hostID)
		if err != nil {
			last = "probe failed: " + truncate(err.Error(), 150)
			continue
		}
		st = got
		if last = EvaluateEngineHealth(got); last == "" {
			return st, ""
		}
	}
	return st, last
}
