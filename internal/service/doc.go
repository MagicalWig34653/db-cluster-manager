// SPDX-License-Identifier: AGPL-3.0-only

// Package service is the service-type framework of the manager (W07, scope
// extension X01, X03, X06, X07, X12; ADR 0012). A service type turns a
// validated, typed desired state into an ordered, human-readable plan, a
// health verdict and a deletion plan. It is pure domain code: no Docker, no
// network, no clock, standard library only; it does not import
// internal/agent. internal/orchestrate converts its service specs into agent
// specs and internal/runner converts its plans into operations.
//
// Status: implemented and tested with fakes only. Nothing here has run
// against a real Swarm, Docker or Traefik.
//
// # Types
//
// The registry holds the types "container" (a generic Docker service, X06)
// and "traefik" (a Traefik reverse proxy with a typed routing model, X07).
// Galera is not a service type yet: it keeps its own domain package and
// safety logic (internal/galera) unchanged; expressing it as a type is later
// W07 work.
//
// # Rules every type follows
//
//   - Specs are strict JSON (unknown fields are refused) and carry no secret
//     values: secrets are Swarm secrets referenced by name, environment
//     variables with secret-looking names must use the "_FILE" convention.
//   - Container images must be pinned to a tag other than "latest" or a
//     digest.
//   - Volumes are managed local volumes below the agent's volume base with an
//     identity marker (A10). A service with volumes is pinned to one host and
//     has one replica. Host paths cannot be bind-mounted.
//   - Every plan that changes state requires approval; destructive steps wait
//     for an approval bound to the digest of the planned operation.
//   - A plan lists Blockers for anything the current agent contract cannot do
//     (published ports, delivery of configuration files). A plan with blockers
//     is shown but never started.
//
// Compatibility drift between the domain spec, the agent spec and the agent
// command kinds is pinned by agentcompat_test.go.
package service
