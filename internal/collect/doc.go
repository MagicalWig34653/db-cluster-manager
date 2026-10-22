// SPDX-License-Identifier: AGPL-3.0-only

// Package collect turns observations into samples for the metric store
// (internal/metrics) and schedules the collection.
//
// Collectors:
//
//   - GaleraCollector: parsed SHOW GLOBAL STATUS (wsrep and server variables)
//     of database nodes, delivered through a StatusSource.
//   - HostCollector: host and container resources, delivered through a
//     StatsSource (Docker stats).
//   - ProxyCollector: the JSON document served at /status by the routing
//     proxy.
//
// Rules shared by all collectors (documented behavior):
//
//   - The catalog (Catalog) lists every metric the package can emit. Names
//     start with "dcm_".
//   - Series are identified by the labels swarm, cluster, node (and host,
//     container, role for resource and proxy metrics). Label values come from
//     configured targets, never from free-form observed text. Every collector
//     has a hard target limit; excess targets are dropped and counted.
//   - Missing data stays missing. A failed or stale observation produces no
//     value samples, only dcm_target_up = 0, so queries show a gap and alert
//     rules evaluate to "unknown". Zero is never invented for an unparsable
//     or absent value.
//   - Counters are exported as the raw monotonic values of the source
//     (suffix _total); resets are handled by the query layer.
//
// The Scheduler runs collectors with an injected clock, per-collector
// timeouts, deterministic jitter, at most one run in flight per collector,
// backoff when the store pushes back, and its own self-metrics.
//
// Series-to-resource mapping (ResourceOf, SeriesAuthorizer) is the single
// place that decides which permission is needed to read a series.
package collect
