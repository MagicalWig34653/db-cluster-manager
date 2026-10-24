// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
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
	// Constraints and Replicas describe a service's placement and scale;
	// update-service compares them to refuse changes it must not make.
	Constraints []string `json:"constraints,omitempty"` // services only
	Replicas    uint64   `json:"replicas,omitempty"`    // replicated services only
	// UpdateState is the Swarm UpdateStatus.State of a service ("" when it
	// was never updated, "updating", "completed", "paused", "rollback_...").
	UpdateState string `json:"update_state,omitempty"` // services only
	// RunningTasks is the Swarm ServiceStatus.RunningTasks of a service when
	// the daemon reported it (services listed with status); nil: unknown.
	RunningTasks *uint64 `json:"running_tasks,omitempty"` // services only
}

// VolumeSpec describes a local Docker volume whose data lives in the host
// directory Device (driver local, options type=none, o=bind). The directory
// must already exist.
type VolumeSpec struct {
	Name   string
	Labels map[string]string
	Device string
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
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// User overrides the image user ("uid:gid"); empty keeps the image's.
	User          string         `json:"user,omitempty"`
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
	// Ports are the ports published on the Swarm. Only published ports are
	// expressible: an unpublished port needs no Swarm configuration.
	Ports []PortSpec `json:"ports,omitempty"`
	// Configs are references to existing Swarm configs, mounted as files.
	// Like Secrets they must pass the ownership guard and are pinned to the
	// object ID that was checked.
	Configs []ConfigRef `json:"configs,omitempty"`
	// UpdateConfig and RollbackConfig are the Swarm rolling-update and
	// rollback behaviour. Nil sends nothing: on create Docker applies its
	// defaults, on update-service the service keeps its current value
	// (dockerclient preserves unmodeled keys), so specs without them, such
	// as every Galera spec, behave exactly as before.
	UpdateConfig   *UpdateConfig `json:"update_config,omitempty"`
	RollbackConfig *UpdateConfig `json:"rollback_config,omitempty"`
}

// UpdateConfig is a Swarm UpdateConfig or RollbackConfig.
type UpdateConfig struct {
	// Parallelism is the number of tasks updated at once; at least 1 (the
	// Swarm value 0, "all at once", is refused).
	Parallelism uint64 `json:"parallelism"`
	// DelayNS is the pause between task batches.
	DelayNS int64 `json:"delay_ns,omitempty"`
	// FailureAction is "pause", "continue" or (update only) "rollback".
	FailureAction string `json:"failure_action,omitempty"`
	// MonitorNS is how long Swarm watches each updated task for a failure
	// (including a failing container health check) before it counts it as
	// updated.
	MonitorNS int64 `json:"monitor_ns,omitempty"`
	// MaxFailureRatio is the tolerated share of failed tasks, 0..1.
	MaxFailureRatio float64 `json:"max_failure_ratio,omitempty"`
	// Order is "stop-first" or "start-first"; empty is the Docker default
	// (stop-first).
	Order string `json:"order,omitempty"`
}

// Update orders and failure actions of a Swarm UpdateConfig.
const (
	UpdateStopFirst       = "stop-first"
	UpdateStartFirst      = "start-first"
	FailureActionPause    = "pause"
	FailureActionContinue = "continue"
	FailureActionRollback = "rollback"
)

// CheckUpdateConfig validates an update (rollback=false) or rollback
// config. A nil config is valid.
func CheckUpdateConfig(u *UpdateConfig, rollback bool) error {
	if u == nil {
		return nil
	}
	if u.Parallelism < 1 || u.Parallelism > 1000 {
		return fmt.Errorf("parallelism %d out of range 1..1000", u.Parallelism)
	}
	if u.DelayNS < 0 || u.MonitorNS < 0 {
		return fmt.Errorf("negative delay or monitor period")
	}
	if u.MaxFailureRatio < 0 || u.MaxFailureRatio > 1 || u.MaxFailureRatio != u.MaxFailureRatio {
		return fmt.Errorf("max_failure_ratio %v out of range 0..1", u.MaxFailureRatio)
	}
	switch u.FailureAction {
	case "", FailureActionPause, FailureActionContinue:
	case FailureActionRollback:
		if rollback {
			return fmt.Errorf("a rollback config cannot have failure_action rollback")
		}
	default:
		return fmt.Errorf("failure_action %q (want pause, continue or rollback)", u.FailureAction)
	}
	switch u.Order {
	case "", UpdateStopFirst, UpdateStartFirst:
	default:
		return fmt.Errorf("order %q (want stop-first or start-first)", u.Order)
	}
	return nil
}

// Port protocols and publish modes of a PortSpec.
const (
	PortProtocolTCP = "tcp"
	PortProtocolUDP = "udp"
	PortModeIngress = "ingress"
	PortModeHost    = "host"
)

// PortSpec publishes a container port on the Swarm.
type PortSpec struct {
	// Target is the container port (1..65535).
	Target int `json:"target"`
	// Published is the port on the Swarm (1..65535).
	Published int `json:"published"`
	// Protocol is "tcp" (default) or "udp".
	Protocol string `json:"protocol,omitempty"`
	// Mode is "ingress" (default, routing mesh) or "host".
	Mode string `json:"mode,omitempty"`
}

// ConfigRef mounts a Swarm config as a read-only file. The config's content
// is not secret; credentials belong in Secrets.
type ConfigRef struct {
	// Config is the name or ID of the config.
	Config string `json:"config"`
	// Target is the absolute file path in the container.
	Target string `json:"target"`
	// Mode is the file mode (default 0444).
	Mode uint32 `json:"mode,omitempty"`
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

// ErrConflict is returned (wrapped) by DockerAPI implementations when the
// daemon refused a request because of the object's current state (HTTP
// 409), for example a volume still mounted by a container. The request had
// no effect.
var ErrConflict = errors.New("agent: docker conflict")

// DockerAPI is the narrow set of Docker operations the executor uses. It is
// implemented by internal/dockerclient (Docker Engine API) and by a test
// fake.
type DockerAPI interface {
	CreateConfig(context.Context, ConfigSpec) (string, error)
	RemoveConfig(context.Context, string) error
	CreateService(ctx context.Context, spec ServiceSpec) (id string, err error)
	RemoveService(ctx context.Context, id string) error
	ScaleService(ctx context.Context, id string, replicas uint64) error
	RemoveVolume(ctx context.Context, id string) error
	// RemoveSecret removes a Swarm secret by ID (a secret still used by a
	// service answers with ErrConflict).
	RemoveSecret(ctx context.Context, id string) error
	// CreateVolume creates the volume. Creating a volume that already exists
	// with the same options succeeds (Docker semantics).
	CreateVolume(ctx context.Context, spec VolumeSpec) error
	// UpdateService replaces the service's spec. Implementations read the
	// current version themselves and fail if it changed concurrently.
	UpdateService(ctx context.Context, id string, spec ServiceSpec) error
	Inspect(ctx context.Context, kind ObjectKind, id string) (Object, error)
	ListByLabel(ctx context.Context, kind ObjectKind, key, value string) ([]Object, error)
}
