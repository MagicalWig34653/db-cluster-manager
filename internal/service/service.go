// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Errors of the framework.
var (
	// ErrInvalidSpec: the spec or request is invalid. The wrapped text names
	// every problem found; it never contains secret values (specs have none).
	ErrInvalidSpec = errors.New("service: invalid spec")
	// ErrUnknownType: no service type of that name is registered.
	ErrUnknownType = errors.New("service: unknown service type")
	// ErrNotExecutable: the plan has blockers and cannot be started.
	ErrNotExecutable = errors.New("service: plan is not executable with the current agent contract")
)

// PlanKind is the kind of a plan.
type PlanKind string

// Plan kinds.
const (
	PlanCreate PlanKind = "create"
	PlanDelete PlanKind = "delete"
)

// StepKind is the kind of a plan step. The kinds that map to an agent
// command have the command's name (agentcompat_test.go).
type StepKind string

// Step kinds.
const (
	StepCreateVolume  StepKind = "create-volume"
	StepInitIdentity  StepKind = "init-identity"
	StepCreateService StepKind = "create-service"
	StepRemoveService StepKind = "remove-cluster-service"
	StepRemoveVolume  StepKind = "remove-volume"
	// StepDeliverConfig places a rendered configuration file for a service.
	// No agent command exists for it, so a plan that contains it has a
	// blocker.
	StepDeliverConfig StepKind = "deliver-config"
)

// Step is one step of a plan, in domain terms (no agent types).
type Step struct {
	ID   string   `json:"id"`
	Kind StepKind `json:"kind"`
	// HostID is the Swarm node the step runs on. Empty for service steps
	// means "any Swarm manager node": the caller binds the service host.
	HostID string `json:"hostId,omitempty"`
	// After lists step IDs that must have succeeded first.
	After []string `json:"after,omitempty"`
	// Volume and NodeUUID identify the managed volume of volume steps.
	Volume   string `json:"volume,omitempty"`
	NodeUUID string `json:"nodeUuid,omitempty"`
	// Spec is the service of create-service steps.
	Spec *ServiceSpec `json:"spec,omitempty"`
	// ServiceName and ServiceID name the service of remove steps.
	ServiceName string `json:"serviceName,omitempty"`
	ServiceID   string `json:"serviceId,omitempty"`
	// Artifact names the artifact a deliver-config step places.
	Artifact string `json:"artifact,omitempty"`
	// Destructive steps remove or overwrite state.
	Destructive bool `json:"destructive,omitempty"`
	// Note is the step in words, for the approval.
	Note string `json:"note"`
}

// Artifact is a generated document the user can review before approving
// (X03): the service spec, rendered configuration. Content never contains
// secret values.
type Artifact struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Content   string `json:"content"`
}

// Plan is an ordered, explainable plan. Executing it is elsewhere
// (internal/runner through manager.Operations).
type Plan struct {
	Kind      PlanKind   `json:"kind"`
	Type      string     `json:"type"`
	ServiceID string     `json:"serviceId"`
	Name      string     `json:"name"`
	Steps     []Step     `json:"steps"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Warnings are shown to the user; they do not stop the plan.
	Warnings []string `json:"warnings,omitempty"`
	// Blockers are things the current agent contract cannot do. A plan with
	// blockers is previewed but must not be started.
	Blockers []string `json:"blockers,omitempty"`
	// RequiresApproval is true for every plan: it changes state.
	RequiresApproval bool `json:"requiresApproval"`
}

// Executable reports whether the plan has no blockers.
func (p Plan) Executable() bool { return len(p.Blockers) == 0 }

// Text renders the plan for the approval dialog: a numbered step list,
// then warnings and blockers.
func (p Plan) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s service %q (id %s)\n", p.Kind, p.Type, p.Name, p.ServiceID)
	for i, s := range p.Steps {
		mark := ""
		if s.Destructive {
			mark = " [destructive]"
		}
		fmt.Fprintf(&b, "%d. %s: %s%s\n", i+1, s.Kind, s.Note, mark)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	for _, w := range p.Blockers {
		fmt.Fprintf(&b, "blocked: %s\n", w)
	}
	return b.String()
}

// Host is the planner's view of a Swarm node.
type Host struct {
	ID     string
	Name   string
	Ready  bool
	Active bool
}

// Context is the identity every call needs.
type Context struct {
	InstallationID string
	// ServiceID is the manager-issued ID; it becomes the ownership label
	// value and the identity of the service's volumes.
	ServiceID string
	Name      string
}

// PlanInput is the input of Plan and DeletePlan.
type PlanInput struct {
	Context
	// Spec is the type-specific desired state (strict JSON).
	Spec json.RawMessage
	// Hosts are the enrolled hosts of the Swarm. A placement on a host that
	// is not listed (or not ready and active) is refused. A nil list skips
	// the check (pure tests only).
	Hosts []Host
	// RemoveVolumes (delete plans) also removes the managed volumes after
	// the agent verified their identity. Default: data is kept.
	RemoveVolumes bool
}

// Observation is what the manager observed about a service in the Swarm.
type Observation struct {
	// Known is false when nothing was observed (agent unreachable, no
	// inventory yet). Health is then "unknown", never "healthy".
	Known bool
	// Running and Desired are the task counts of the service.
	Running int
	Desired int
	// Reason is an optional observed failure text (not secret).
	Reason string
}

// HealthState is a verdict.
type HealthState string

// Health states. Unknown is distinct from unhealthy.
const (
	HealthHealthy   HealthState = "healthy"
	HealthDegraded  HealthState = "degraded"
	HealthUnhealthy HealthState = "unhealthy"
	HealthUnknown   HealthState = "unknown"
)

// Health is the verdict for a service.
type Health struct {
	State   HealthState `json:"state"`
	Reasons []string    `json:"reasons,omitempty"`
}

// HealthInput is the input of Health.
type HealthInput struct {
	Context
	Spec     json.RawMessage
	Observed Observation
}

// Type is a service type. Implementations are pure.
type Type interface {
	// Name is the registry key, for example "container".
	Name() string
	// Validate checks a spec without planning.
	Validate(spec json.RawMessage) error
	// Plan returns the create plan: ordered steps, the generated
	// artifacts, warnings and blockers.
	Plan(in PlanInput) (Plan, error)
	// Health judges an observation against the spec.
	Health(in HealthInput) (Health, error)
	// DeletePlan returns the plan that removes the service. It needs only
	// the spec, never observed state, so a failed create can be discarded.
	DeletePlan(in PlanInput) (Plan, error)
}

// Registry maps type names to types.
type Registry struct{ types map[string]Type }

// NewRegistry returns a registry of types; names must be unique and set.
func NewRegistry(types ...Type) (*Registry, error) {
	r := &Registry{types: map[string]Type{}}
	for _, t := range types {
		n := t.Name()
		if n == "" {
			return nil, errors.New("service: type without a name")
		}
		if _, dup := r.types[n]; dup {
			return nil, fmt.Errorf("service: type %q registered twice", n)
		}
		r.types[n] = t
	}
	return r, nil
}

// DefaultRegistry returns the registry of the built-in types.
func DefaultRegistry() *Registry {
	r, err := NewRegistry(Container{}, Traefik{}, SFTP{}, FTP{}, S3{}, NFS{})
	if err != nil {
		panic(err) // the built-in names are distinct
	}
	return r
}

// Get returns the type of that name.
func (r *Registry) Get(name string) (Type, error) {
	t, ok := r.types[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, name)
	}
	return t, nil
}

// Names returns the registered type names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.types))
	for n := range r.types {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Naming. Names are checked by ValidName.

// NamePrefix prefixes the Swarm service and volume names of this framework.
// Galera services are named "dbcm-<cluster>-<slot>"; the ownership label,
// not the name, is what the agent trusts.
const NamePrefix = "dbcm-svc-"

// SwarmServiceName is the Swarm service name of a service.
func SwarmServiceName(name string) string { return NamePrefix + name }

// VolumeName is the Docker volume name of a managed volume.
func VolumeName(name, vol string) string { return NamePrefix + name + "-" + vol }

// VolumeIdentity is the node UUID written into the identity marker of a
// managed volume. It is derived from the service ID and the volume name, so
// a deletion plan can verify the identity without stored state.
func VolumeIdentity(serviceID, vol string) string {
	h := sha256.Sum256([]byte("dbcm-service-volume\x00" + serviceID + "\x00" + vol))
	x := h[:16]
	x[6] = x[6]&0x0f | 0x50 // version 5 style
	x[8] = x[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
}

// decodeStrict decodes JSON into v and refuses unknown fields and
// trailing data.
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%w: empty spec", ErrInvalidSpec)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data after the spec", ErrInvalidSpec)
	}
	return nil
}

// problems collects validation messages.
type problems []string

func (p *problems) addf(format string, a ...any) { *p = append(*p, fmt.Sprintf(format, a...)) }

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidSpec, strings.Join(p, "; "))
}

func (c Context) check() error {
	var p problems
	if c.InstallationID == "" {
		p.addf("installation ID missing")
	}
	if c.ServiceID == "" {
		p.addf("service ID missing")
	}
	if !validName(c.Name) {
		p.addf("service name %q must match %s", c.Name, nameRe)
	}
	return p.err()
}
