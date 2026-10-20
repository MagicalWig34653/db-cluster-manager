// SPDX-License-Identifier: AGPL-3.0-only

// Package orchestrate connects the database domain packages to the agent
// command contract. The domain packages (internal/galera) stay independent
// of internal/agent; this package converts their output into the specs the
// agent executes.
package orchestrate

import (
	"maps"
	"math"
	"slices"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// NanoCPUsPerMilliCPU converts galera's thousandths of a CPU into Docker's
// NanoCPUs (1 CPU = 1e9 NanoCPUs = 1000 milli-CPUs).
const NanoCPUsPerMilliCPU = 1_000_000

// MilliToNanoCPUs converts milli-CPUs to NanoCPUs. Values that would
// overflow saturate at math.MaxInt64; non-positive values give 0 (unset).
func MilliToNanoCPUs(milli int64) int64 {
	switch {
	case milli <= 0:
		return 0
	case milli > math.MaxInt64/NanoCPUsPerMilliCPU:
		return math.MaxInt64
	}
	return milli * NanoCPUsPerMilliCPU
}

// ToAgentSpec converts a Galera node service spec into the spec of an agent
// create_service command. The result shares no slices or maps with s.
//
// Empty RestartCondition gives no restart policy and zero resources give no
// resources block, so Docker defaults apply. Validation stays with the
// producers (galera.BuildServiceSpec) and the consumers (agent command
// decoding, dockerclient); the conversion itself does not drop or rewrite
// values.
func ToAgentSpec(s galera.ServiceSpec) agent.ServiceSpec {
	out := agent.ServiceSpec{
		Name:              s.Name,
		Image:             s.Image,
		Labels:            maps.Clone(s.Labels),
		Env:               slices.Clone(s.Env),
		Args:              slices.Clone(s.Args),
		Constraints:       slices.Clone(s.Constraints),
		Replicas:          s.Replicas,
		Hostname:          s.Hostname,
		Networks:          slices.Clone(s.Networks),
		EndpointMode:      s.EndpointMode,
		Secrets:           slices.Clone(s.Secrets),
		StopGracePeriodNS: s.StopGracePeriod.Nanoseconds(),
		Resources:         toAgentResources(s.Resources),
	}
	for _, m := range s.Mounts {
		out.Mounts = append(out.Mounts, agent.Mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if s.RestartCondition != "" {
		out.RestartPolicy = &agent.RestartPolicy{Condition: s.RestartCondition}
	}
	return out
}

func toAgentResources(r galera.Resources) *agent.Resources {
	var out agent.Resources
	if l := r.Limits; l != (galera.ResourceAmount{}) {
		out.Limits = &agent.ResourceLimits{NanoCPUs: MilliToNanoCPUs(l.MilliCPU), MemoryBytes: l.MemoryBytes}
	}
	if v := r.Reservations; v != (galera.ResourceAmount{}) {
		out.Reservations = &agent.ResourceReservations{NanoCPUs: MilliToNanoCPUs(v.MilliCPU), MemoryBytes: v.MemoryBytes}
	}
	if out.Limits == nil && out.Reservations == nil {
		return nil
	}
	return &out
}
