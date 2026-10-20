// SPDX-License-Identifier: AGPL-3.0-only

// Package galera holds the MariaDB Galera domain logic of the manager: Swarm
// service definitions for Galera nodes, placement and the cluster creation
// plan, the automatic single-node replacement policy, full-outage diagnosis,
// and node health from wsrep status variables. It is pure: no Docker, no
// network, no clock reads (time is an input), standard library only. It
// produces plans; executing them (agent commands, fencing, journaling) is
// elsewhere.
//
// Sources: docs/KONZEPT.md A01-A06 and A08, docs/ARCHITEKTUR.md "Local
// storage and recovery", ADR 0004 and its lab evidence (E1/E2,
// tests/p0/results). The service options are the ones that worked in E1/E2
// with mariadb:11.4 (11.4.13, Galera 4, arm64).
//
// # Service specs
//
// One service per node, replicas 1, pinned with node.id==<host>, labels
// dbcm.installation/cluster/node (node = node UUID), endpoint mode dnsrr,
// restart condition any, stop grace period 60s. The node's named volume is
// mounted once at /dbcm; the identity marker is /dbcm/dbcm-identity.json and
// the datadir /dbcm/data (--datadir), so SST cannot delete the marker.
// BootstrapFlag (--wsrep-new-cluster) is emitted only when SpecOptions
// requests it; every spec stored as desired state passes
// ServiceSpec.ValidateSteadyState.
//
// # Placement precedence (A01)
//
// Strongest first: allowlist > exclusion > preference > automatic scoring.
//
//  1. A non-empty allowlist is the strict candidate set, without fallback.
//  2. Excluded hosts are never used. A host that is both allowlisted and
//     excluded is rejected as ErrPolicyConflict, not resolved silently.
//  3. Preferred hosts (in list order) come before all others; an excluded
//     or non-allowlisted preferred host is ignored with a warning.
//  4. Automatic scoring: most free memory after placement, then most free
//     CPU, then host ID.
//
// Hard rules in every mode: host ready and active, at most one node of a
// cluster per host, node demand (per dimension the larger of limit and
// reservation) fits after the configured host reserve (A04) and existing
// allocations. Spare hosts (A02/A03) are never used for creation; for
// replacement they rank after preferred and before ordinary hosts.
//
// # Replacement and recovery
//
// PlanReplacement creates a new identity (node UUID, volume, service) in the
// failed node's slot, and only when fresh observations show a strict
// majority of the last known cluster size as live Primary members agreeing
// on the cluster state UUID plus a Synced donor. Otherwise it returns
// ErrRequiresGuidedRecovery; it never relaxes quorum. DiagnoseFullOutage
// proposes a bootstrap candidate from grastate.dat and wsrep-recover
// evidence, lists risks, and always requires approval.
package galera
