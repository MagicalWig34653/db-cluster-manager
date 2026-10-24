// SPDX-License-Identifier: AGPL-3.0-only

package orchestrate

import (
	"slices"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// ServiceToAgentSpec converts a service-framework spec into the spec of an
// agent create-service command. The result shares no slices or maps with s.
// Ports without a published port are internal to the overlay network and
// need no Swarm configuration, so only published ports are converted.
// Validation stays with the service types and the agent; the conversion
// does not drop or rewrite other values.
func ServiceToAgentSpec(s service.ServiceSpec) (agent.ServiceSpec, error) {
	out := agent.ServiceSpec{
		Name:              s.Name,
		Image:             s.Image,
		Labels:            cloneMap(s.Labels),
		Env:               slices.Clone(s.Env),
		Command:           slices.Clone(s.Command),
		Args:              slices.Clone(s.Args),
		User:              s.User,
		Constraints:       slices.Clone(s.Constraints),
		Replicas:          s.Replicas,
		Networks:          slices.Clone(s.Networks),
		Secrets:           slices.Clone(s.Secrets),
		StopGracePeriodNS: s.StopGracePeriod.Nanoseconds(),
		Resources:         serviceResources(s.Resources),
		UpdateConfig:      updateConfig(s.UpdateConfig),
		RollbackConfig:    updateConfig(s.RollbackConfig),
	}
	for _, m := range s.Mounts {
		out.Mounts = append(out.Mounts, agent.Mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	for _, p := range s.PublishedPorts() {
		out.Ports = append(out.Ports, agent.PortSpec{Target: p.Target, Published: p.Published, Protocol: p.Protocol, Mode: p.Mode})
	}
	for _, c := range s.Configs {
		out.Configs = append(out.Configs, agent.ConfigRef{Config: c.Config, Target: c.Target, Mode: c.Mode})
	}
	if s.RestartCondition != "" {
		out.RestartPolicy = &agent.RestartPolicy{Condition: s.RestartCondition}
	}
	return out, nil
}

func updateConfig(u *service.SwarmUpdateConfig) *agent.UpdateConfig {
	if u == nil {
		return nil
	}
	return &agent.UpdateConfig{Parallelism: u.Parallelism, DelayNS: u.Delay.Nanoseconds(), FailureAction: u.FailureAction,
		MonitorNS: u.Monitor.Nanoseconds(), MaxFailureRatio: u.MaxFailureRatio, Order: u.Order}
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func serviceResources(r service.Resources) *agent.Resources {
	var out agent.Resources
	if l := r.Limits; l != (service.ResourceAmount{}) {
		out.Limits = &agent.ResourceLimits{NanoCPUs: MilliToNanoCPUs(l.MilliCPU), MemoryBytes: l.MemoryBytes}
	}
	if v := r.Reservations; v != (service.ResourceAmount{}) {
		out.Reservations = &agent.ResourceReservations{NanoCPUs: MilliToNanoCPUs(v.MilliCPU), MemoryBytes: v.MemoryBytes}
	}
	if out.Limits == nil && out.Reservations == nil {
		return nil
	}
	return &out
}
