// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
)

// Ownership labels from ADR 0004 item 1, plus labels that link an object to
// the operation step that created it (for later reconciliation).
const (
	LabelInstallation = "dbcm.installation"
	LabelCluster      = "dbcm.cluster"
	LabelNode         = "dbcm.node"
	LabelOpID         = "dbcm.op"
	LabelStepID       = "dbcm.step"
)

// ObjectKind is the type of a Docker object.
type ObjectKind string

// Docker object kinds the agent knows.
const (
	KindService   ObjectKind = "service"
	KindVolume    ObjectKind = "volume"
	KindContainer ObjectKind = "container"
	KindNetwork   ObjectKind = "network"
	KindSecret    ObjectKind = "secret"
	KindConfig    ObjectKind = "config"
)

// Object is the subset of a Docker object the agent needs.
type Object struct {
	Kind       ObjectKind        `json:"kind"`
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"` // volumes only
	Mounts     []Mount           `json:"mounts,omitempty"`     // services only
}

// Mount is a service mount.
type Mount struct {
	Type     string `json:"type"` // "bind" or "volume"
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ServiceSpec is the subset of a Swarm service spec the agent creates.
//
// Every field after Replicas is an optional addition and omitempty: a spec
// that does not use them encodes and digests exactly as before (pinned by
// TestServiceSpecEncodingStable).
//
// Networks and Secrets are references (name or ID) to existing Swarm
// objects. The executor only lets a spec reference networks and secrets
// that pass the ownership guard, and it pins each reference to the object
// ID it checked. Secret contents never travel in a spec.
type ServiceSpec struct {
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
	Env    []string          `json:"env,omitempty"`
	// Command overrides the image entrypoint; Args are its arguments.
	Command       []string       `json:"command,omitempty"`
	Args          []string       `json:"args,omitempty"`
	Mounts        []Mount        `json:"mounts,omitempty"`
	Constraints   []string       `json:"constraints,omitempty"` // for example "node.id==abc"
	Replicas      uint64         `json:"replicas"`
	RestartPolicy *RestartPolicy `json:"restart_policy,omitempty"`
	Resources     *Resources     `json:"resources,omitempty"`
	// Hostname is the container hostname (RFC 1123).
	Hostname string `json:"hostname,omitempty"`
	// Networks are the networks the tasks attach to.
	Networks []string `json:"networks,omitempty"`
	// EndpointMode is "" (Docker default), "vip" or "dnsrr".
	EndpointMode string `json:"endpoint_mode,omitempty"`
	// Secrets are mounted as /run/secrets/<secret name>, root-owned, 0444.
	Secrets []string `json:"secrets,omitempty"`
	// StopGracePeriodNS is the time between SIGTERM and SIGKILL in
	// nanoseconds. Zero means the Docker default.
	StopGracePeriodNS int64 `json:"stop_grace_period_ns,omitempty"`
}

// Endpoint modes of a Swarm service.
const (
	EndpointModeVIP   = "vip"
	EndpointModeDNSRR = "dnsrr"
)

// RestartPolicy is a Swarm task restart policy. Zero values mean the Docker
// default.
type RestartPolicy struct {
	Condition   string `json:"condition,omitempty"` // "none", "on-failure" or "any"
	DelayNS     int64  `json:"delay_ns,omitempty"`
	MaxAttempts int64  `json:"max_attempts,omitempty"`
	WindowNS    int64  `json:"window_ns,omitempty"`
}

// Resources are per-task resource limits and reservations.
type Resources struct {
	Limits       *ResourceLimits       `json:"limits,omitempty"`
	Reservations *ResourceReservations `json:"reservations,omitempty"`
}

// ResourceLimits caps a task's resources. Zero means unlimited.
type ResourceLimits struct {
	NanoCPUs    int64 `json:"nano_cpus,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	Pids        int64 `json:"pids,omitempty"`
}

// ResourceReservations are resources the scheduler reserves for a task.
type ResourceReservations struct {
	NanoCPUs    int64 `json:"nano_cpus,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

// ErrObjectNotFound is returned by DockerAPI implementations when an object
// does not exist.
var ErrObjectNotFound = errors.New("agent: docker object not found")

// ErrOutcomeUnknown may be returned (wrapped) by DockerAPI implementations
// when a request may or may not have taken effect, for example after a
// timeout. The executor records such steps as StatusUnknown.
var ErrOutcomeUnknown = errors.New("agent: docker outcome unknown")

// DockerAPI is the narrow set of Docker operations the executor uses. It is
// implemented by internal/dockerclient (Docker Engine API) and by a test
// fake.
type DockerAPI interface {
	CreateService(ctx context.Context, spec ServiceSpec) (id string, err error)
	RemoveService(ctx context.Context, id string) error
	ScaleService(ctx context.Context, id string, replicas uint64) error
	RemoveVolume(ctx context.Context, id string) error
	Inspect(ctx context.Context, kind ObjectKind, id string) (Object, error)
	ListByLabel(ctx context.Context, kind ObjectKind, key, value string) ([]Object, error)
}
