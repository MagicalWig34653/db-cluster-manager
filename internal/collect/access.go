// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metrics"
)

// ResourceOf maps a series to the resource whose read permission is needed to
// see it: a cluster-scoped series needs cluster read, a host series
// (swarm+host, no cluster) needs host read, a swarm-only series needs swarm
// read, and everything else (manager-level, scheduler self-metrics) needs
// settings read. Unknown label shapes therefore fall to the most restrictive
// common resource instead of being public.
func ResourceOf(l metrics.Labels) authz.Resource {
	swarm, cluster := l.Get(LabelSwarm), l.Get(LabelCluster)
	switch {
	case swarm != "" && cluster != "":
		return authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(swarm, cluster)}
	case swarm != "" && l.Get(LabelHost) != "":
		return authz.Resource{Type: authz.ResourceHost, Scope: authz.SwarmScope(swarm), ID: l.Get(LabelHost)}
	case swarm != "":
		return authz.Resource{Type: authz.ResourceSwarm, Scope: authz.SwarmScope(swarm)}
	}
	return authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}
}

// SeriesAuthorizer returns the metrics.Authorizer of one subject. A
// cluster label without a swarm label (an invalid scope) is denied.
func SeriesAuthorizer(c authz.Checker, s authz.Subject) metrics.Authorizer {
	return func(l metrics.Labels) bool {
		if l.Get(LabelCluster) != "" && l.Get(LabelSwarm) == "" {
			return false
		}
		return c.Check(s, authz.ActionRead, ResourceOf(l)).Allowed
	}
}

// SystemSubject is the identity the manager uses for internal reads such as
// alert evaluation. It must be bound to a role with read access.
var SystemSubject = authz.Subject{ID: "manager", Kind: authz.SubjectSystem}
