// SPDX-License-Identifier: AGPL-3.0-only

package service

import "time"

// Ownership labels of the objects of a service. The agent's ownership guard
// calls LabelCluster the cluster label; for service types its value is the
// service ID. The value equals agent.LabelCluster (agentcompat_test.go).
const (
	LabelCluster     = "dbcm.cluster"
	LabelNode        = "dbcm.node"
	LabelServiceType = "dbcm.service-type"
	LabelServiceName = "dbcm.service-name"
)

// Container paths and file names shared with the agent's volume rules.
const (
	// MarkerFileName is the identity marker at a volume root (same as
	// agent.MarkerFileName).
	MarkerFileName = "dbcm-identity.json"
	// SecretsDir is where Swarm mounts secrets in the container.
	SecretsDir = "/run/secrets"
)

// Restart conditions of a Swarm task.
const (
	RestartAny       = "any"
	RestartOnFailure = "on-failure"
	RestartNone      = "none"
)

// Mount is a service mount; it mirrors agent.Mount. Service types only
// produce "volume" mounts of managed volumes.
type Mount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Port is a container port, optionally published on the Swarm. Published
// ports are converted into agent.PortSpec; unpublished ports stay internal.
type Port struct {
	// Target is the container port.
	Target int `json:"target"`
	// Published is the port on the Swarm; 0 keeps the port internal.
	Published int `json:"published,omitempty"`
	// Protocol is "tcp" (default) or "udp".
	Protocol string `json:"protocol,omitempty"`
	// Mode is "ingress" (default, routing mesh) or "host".
	Mode string `json:"mode,omitempty"`
}

// ConfigRef mounts an existing Swarm config as a file. The content is not
// secret (credentials are Swarm secrets). Mirrors agent.ConfigRef.
type ConfigRef struct {
	Config string `json:"config"`
	Target string `json:"target"`
	Mode   uint32 `json:"mode,omitempty"`
}

// ResourceAmount is CPU in thousandths of a CPU and memory in bytes.
type ResourceAmount struct {
	MilliCPU    int64 `json:"milli_cpu,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

// Resources are Swarm task limits and reservations. Zero means unset.
type Resources struct {
	Limits       ResourceAmount `json:"limits"`
	Reservations ResourceAmount `json:"reservations"`
}

// ServiceSpec is a Swarm service definition in domain units.
// internal/orchestrate.ServiceToAgentSpec converts it into an
// agent.ServiceSpec; agentcompat_test.go fails when a field is added here
// without being converted (or declared domain-only).
type ServiceSpec struct {
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Labels           map[string]string `json:"labels,omitempty"`
	Env              []string          `json:"env,omitempty"`
	Command          []string          `json:"command,omitempty"`
	Args             []string          `json:"args,omitempty"`
	User             string            `json:"user,omitempty"`
	Mounts           []Mount           `json:"mounts,omitempty"`
	Ports            []Port            `json:"ports,omitempty"`
	Constraints      []string          `json:"constraints,omitempty"`
	Replicas         uint64            `json:"replicas"`
	Networks         []string          `json:"networks,omitempty"`
	Secrets          []string          `json:"secrets,omitempty"`
	Configs          []ConfigRef       `json:"configs,omitempty"`
	RestartCondition string            `json:"restart_condition,omitempty"`
	StopGracePeriod  time.Duration     `json:"stop_grace_period,omitempty"`
	Resources        Resources         `json:"resources"`
	// UpdateConfig and RollbackConfig are the Swarm rolling-update and
	// rollback behaviour (update.go). Nil leaves the Docker default on
	// create and the current value on update.
	UpdateConfig   *SwarmUpdateConfig `json:"update_config,omitempty"`
	RollbackConfig *SwarmUpdateConfig `json:"rollback_config,omitempty"`
}

// SwarmUpdateConfig mirrors agent.UpdateConfig in domain units.
type SwarmUpdateConfig struct {
	Parallelism     uint64        `json:"parallelism"`
	Delay           time.Duration `json:"delay,omitempty"`
	FailureAction   string        `json:"failure_action,omitempty"`
	Monitor         time.Duration `json:"monitor,omitempty"`
	MaxFailureRatio float64       `json:"max_failure_ratio,omitempty"`
	Order           string        `json:"order,omitempty"`
}

// PublishedPorts returns the ports with a published port.
func (s ServiceSpec) PublishedPorts() []Port {
	var out []Port
	for _, p := range s.Ports {
		if p.Published > 0 {
			out = append(out, p)
		}
	}
	return out
}
