// SPDX-License-Identifier: AGPL-3.0-only

// Package agentrt makes the runtime of every agent explicit (ADR 0024).
//
// The product has two agent runtimes: the Docker container agent (a Swarm
// global service, default for Swarm objects) and the native agent (a systemd
// service on the host, ADR 0023). Each agent reports its runtime and a
// capability list at enrollment, in every response and in its health
// answers. The manager keeps the report per host and routes each operation
// step through the explicit table in this package. A step whose required
// runtime is absent is refused with a reason; there is no silent fallback.
package agentrt

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Runtime is how an agent executes on its host.
type Runtime string

// Runtimes.
const (
	Native    Runtime = "native"
	Container Runtime = "container"
)

// Capability is a function group an agent may offer.
type Capability string

// Capabilities.
const (
	// DockerObjects: Swarm services, volumes, configs, secrets and jobs
	// through the Docker Engine API (container agent only).
	DockerObjects Capability = "docker-objects"
	// GaleraSteps: identity markers, volume directories, node health and SQL
	// templates on a database volume (container agent only).
	GaleraSteps Capability = "galera-steps"
	// Restic: host filesystem backups with restic (either runtime).
	Restic Capability = "restic"
	// Patch: OS package reports, simulation, apply and reboot (native only).
	Patch Capability = "patch"
	// HostMaintenance and Diagnostics are reserved names. No agent claims
	// them and no step maps to them yet; they exist so a future move of such
	// a function is an explicit table change (ADR 0024).
	HostMaintenance Capability = "host-maintenance"
	Diagnostics     Capability = "diagnostics"
)

// allowed is which runtime may claim which capability.
var allowed = map[Runtime]map[Capability]bool{
	Container: {DockerObjects: true, GaleraSteps: true, Restic: true, Diagnostics: true},
	Native:    {Patch: true, Restic: true, HostMaintenance: true, Diagnostics: true},
}

// Errors.
var (
	// ErrMismatch: an agent reported an unknown runtime or capability, or a
	// capability its runtime cannot have.
	ErrMismatch = errors.New("agentrt: runtime and capabilities do not match")
	// ErrNoRuntime: the required runtime is absent on the host.
	ErrNoRuntime = errors.New("agentrt: required agent runtime not available")
)

// Info is what an agent reports about itself.
type Info struct {
	Runtime      Runtime      `json:"runtime"`
	Capabilities []Capability `json:"capabilities"`
}

// ContainerInfo is the report of the Docker container agent; withRestic is
// true when restic allow-list roots are configured.
func ContainerInfo(withRestic bool) Info {
	c := []Capability{DockerObjects, GaleraSteps}
	if withRestic {
		c = append(c, Restic)
	}
	return Info{Runtime: Container, Capabilities: c}
}

// NativeInfo is the report of the native agent: patch always, restic when
// restic allow-list roots are configured.
func NativeInfo(withRestic bool) Info {
	c := []Capability{Patch}
	if withRestic {
		c = append(c, Restic)
	}
	return Info{Runtime: Native, Capabilities: c}
}

// Has reports whether the capability is claimed.
func (i Info) Has(c Capability) bool { return slices.Contains(i.Capabilities, c) }

// Validate rejects a report whose capabilities its runtime cannot have.
func (i Info) Validate() error {
	caps, ok := allowed[i.Runtime]
	if !ok {
		return fmt.Errorf("%w: unknown runtime %q", ErrMismatch, i.Runtime)
	}
	if len(i.Capabilities) == 0 {
		return fmt.Errorf("%w: %s agent reports no capabilities", ErrMismatch, i.Runtime)
	}
	seen := map[Capability]bool{}
	for _, c := range i.Capabilities {
		switch {
		case seen[c]:
			return fmt.Errorf("%w: duplicate capability %q", ErrMismatch, c)
		case !caps[c]:
			return fmt.Errorf("%w: a %s agent cannot offer %q", ErrMismatch, i.Runtime, c)
		}
		seen[c] = true
	}
	return nil
}

// Response headers carrying the report on every agent answer.
const (
	HeaderRuntime      = "X-Dbcm-Agent-Runtime"
	HeaderCapabilities = "X-Dbcm-Agent-Capabilities"
)

// SetHeaders adds the report to h.
func (i Info) SetHeaders(h http.Header) {
	caps := make([]string, len(i.Capabilities))
	for n, c := range i.Capabilities {
		caps[n] = string(c)
	}
	h.Set(HeaderRuntime, string(i.Runtime))
	h.Set(HeaderCapabilities, strings.Join(caps, ","))
}

// InfoFromHeaders parses and validates a report. present is false when the
// agent sent none (an agent built before ADR 0024).
func InfoFromHeaders(h http.Header) (info Info, present bool, err error) {
	rt, caps := h.Get(HeaderRuntime), h.Get(HeaderCapabilities)
	if rt == "" && caps == "" {
		return Info{}, false, nil
	}
	info.Runtime = Runtime(rt)
	for c := range strings.SplitSeq(caps, ",") {
		if c = strings.TrimSpace(c); c != "" {
			info.Capabilities = append(info.Capabilities, Capability(c))
		}
	}
	return info, true, info.Validate()
}

// Requirement names what a step type needs: a capability, offered by one of
// Runtimes. Runtimes is in order of preference.
type Requirement struct {
	Capability Capability
	Runtimes   []Runtime
}

var (
	containerOnly = []Runtime{Container}
	nativeOnly    = []Runtime{Native}
	// Restic runs on either runtime; the native agent is preferred because
	// it sees the host paths and has an ssh client for SFTP repositories.
	nativeFirst = []Runtime{Native, Container}
)

// PatchKindPrefix prefixes the patch command kinds in the table
// ("patch/" + patch.Cmd*), which are not agent.Command kinds.
const PatchKindPrefix = "patch/"

// table is the explicit capability to runtime mapping (ADR 0024). It is the
// only place that decides which runtime executes a step type. A kind that is
// not listed is refused.
var table = func() map[string]Requirement {
	t := map[string]Requirement{}
	add := func(c Capability, rts []Runtime, kinds ...string) {
		for _, k := range kinds {
			t[k] = Requirement{Capability: c, Runtimes: rts}
		}
	}
	add(DockerObjects, containerOnly,
		"create-service", "remove-service", "start-db", "stop-db", "remove-volume", "inspect", "list-owned",
		"create-volume", "update-service", "allow-objects", "create-config", "remove-config",
		"run-backup", "run-restore", "remove-job", "await-service", "await-stopped", "start-service",
		"remove-cluster-service", "remove-secret", "check-restore-target", "set-deny-list")
	add(GaleraSteps, containerOnly,
		"init-identity", "verify-identity", "collect-health", "read-grastate", "init-volume-dirs",
		"exec-sql", "mark-safe-to-bootstrap", "wsrep-recover")
	add(Restic, nativeFirst,
		"restic-backup", "restic-snapshots", "restic-forget", "restic-check", "restic-restore", "restic-stats")
	add(Patch, nativeOnly,
		PatchKindPrefix+"report", PatchKindPrefix+"simulate", PatchKindPrefix+"apply",
		PatchKindPrefix+"reboot", PatchKindPrefix+"deny-list")
	return t
}()

// RequirementFor returns the requirement of a step type; ok is false for an
// unmapped kind.
func RequirementFor(kind string) (Requirement, bool) {
	r, ok := table[kind]
	return r, ok
}

// Kinds lists every mapped kind (sorted); for documentation and tests.
func Kinds() []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Select picks the runtime for a requirement among the host's reports. It
// walks req.Runtimes in order and takes the first runtime whose report has
// the capability. When none does it returns an ErrNoRuntime error that says
// what is required and what the host reports. It never picks a runtime the
// table does not name.
func Select(req Requirement, have map[Runtime]Info) (Runtime, error) {
	for _, rt := range req.Runtimes {
		if info, ok := have[rt]; ok && info.Has(req.Capability) {
			return rt, nil
		}
	}
	want := make([]string, len(req.Runtimes))
	for n, rt := range req.Runtimes {
		want[n] = string(rt)
	}
	if len(have) == 0 {
		return "", fmt.Errorf("%w: capability %q needs a %s agent, but the host has reported no agent", ErrNoRuntime, req.Capability, strings.Join(want, " or "))
	}
	var got []string
	for _, rt := range []Runtime{Native, Container} {
		if info, ok := have[rt]; ok {
			caps := make([]string, len(info.Capabilities))
			for n, c := range info.Capabilities {
				caps[n] = string(c)
			}
			got = append(got, fmt.Sprintf("%s [%s]", rt, strings.Join(caps, ",")))
		}
	}
	return "", fmt.Errorf("%w: capability %q needs a %s agent; the host reports %s", ErrNoRuntime, req.Capability, strings.Join(want, " or "), strings.Join(got, "; "))
}
