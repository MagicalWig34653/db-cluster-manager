// SPDX-License-Identifier: AGPL-3.0-only

// Package manager is the control-plane core of the manager process. It wires
// the library packages into one replicated service:
//
//   - store.go: a typed, versioned document store on top of the metastore's
//     replicated documents (compare-and-set on Version, local reads with an
//     explicit Consistent() barrier option).
//   - model.go: the desired-state and inventory records (swarms, hosts,
//     clusters, settings, operations, events).
//   - secrets.go: encryption of secret documents at rest with a data key
//     from a KeyProvider. Key provisioning across replicas is NOT solved:
//     every replica must be given the same key out of band.
//   - authstore.go: persistence adapters that keep accounts, sessions,
//     recovery codes, passkeys, roles/bindings and audit records in the
//     replicated store, and AuthStack, which builds the API's auth stores
//     from it.
//   - service.go, leader.go: Service starts an embedded metastore node
//     (single-instance mode is a one-member group) and runs a leader-only
//     loop: on gaining leadership it runs the barrier, takes over unfinished
//     operations with a fresh runner and starts reconcilers; on losing it
//     everything is stopped and agent commands are refused locally.
//   - inventory.go: api.Inventory and api.Activity from the replicated store;
//     missing or stale observations are reported as unknown.
//   - operations.go: permission-checked, audited commands that start runner
//     operations (create cluster, replace node) and the read-only guided
//     full-outage diagnosis.
//   - agents.go: an AgentClient that reaches agents through the lab HTTP
//     endpoint recorded in each host's enrollment record. This endpoint has
//     no TLS and no authentication: it is lab-grade only.
//
// Package swarm (internal/manager/swarm) holds the Swarm connection, the
// Docker compatibility matrix and the agent deployment.
//
// Status: prototype for the R1 work packages. Tested in process and on
// loopback with fakes for Docker and agents; not run against a real Swarm.
package manager
