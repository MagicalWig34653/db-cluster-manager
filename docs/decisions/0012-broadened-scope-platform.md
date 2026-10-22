# ADR 0012: Broadened scope — a general self-hosted cluster and service platform

Date: 2026-10-22
Status: Accepted by the owner for direction. All mechanisms below are Proposed; none is implemented or validated. MariaDB Galera remains priority 1.

## Context

The product was scoped as a manager for database clusters on existing Docker Swarms ([ADR 0001](0001-product-direction.md), [requirements](../KONZEPT.md)). On 2026-10-22 the owner broadened the direction. In summary, the product should:

- optionally build and manage Swarm or Kubernetes clusters, not only database clusters;
- be as intuitive as possible and operable entirely through the UI, without the user losing an understanding of what the system actually does;
- support further server types: web servers, storage protocol servers ((S)FTP, S3, NFS and similar) and generic Docker containers;
- possibly control Traefik directly through the UI;
- include Docker engine updates;
- allow detailed composition of server configurations, for example "Nginx version X with PHP version Y, these PHP extensions/functions and this Nginx configuration", from which the manager builds the image itself and pushes/distributes it either to a container registry optionally operated by the manager or to a configured external registry (for example ghcr).

The owner also stated that Galera currently has priority 1.

The confirmed requirements for this extension are recorded as `X01`–`X12` in [KONZEPT.md](../KONZEPT.md#scope-extension).

## Decision (confirmed direction)

1. Position the product as a general self-hosted platform for clusters and services; database clusters are the first and most mature use case, not the only one.
2. **Priority:** MariaDB Galera on Swarm stays first. The P0 gates and R1 scope and exit gates in [RELEASES.md](../RELEASES.md) are unchanged and must not be delayed or weakened by the extension. Extension work starts only after R1, unless the owner decides otherwise; it may influence R1 only through interface boundaries that are cheap to keep (see "Architecture principle").
3. The owner wants Docker engine updates included. This partly supersedes [A09](../KONZEPT.md#placement-automation-and-host-protection) ("Docker upgrades remain the user's responsibility"). The exact boundary — who restarts hosts, which steps run automatically and which need approval — is an **open decision** in [ENTSCHEIDUNGEN.md](../ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension) and is not decided by this ADR.

## Architecture principle (proposed)

### Service types

Introduce a generic *service type* abstraction. A service type is a declarative, versioned definition that the manager core interprets; Galera becomes the first service type rather than a special case of the core. Further types: web server (composed), storage protocol servers, generic container, Traefik, container registry, and later other database engines.

Each service type declares:

| Element | Purpose |
| --- | --- |
| Desired-state schema | Validated, versioned specification of what the user wants (for example node count, image, ports, volumes, settings). |
| UI form schema | Form/wizard layout, defaults, help texts (DE/EN) and validation messages derived from or bound to the desired-state schema. |
| Planner | Pure function from desired state, observed state and policy to an ordered, explainable plan of steps. |
| Reconcile and health model | Observations, health states (including "unknown"), drift detection and which steps are safe to repeat. |
| Update strategy | Rolling, recreate, blue/green or engine-specific (for example Galera's node-by-node with quorum checks); version paths and approval rules. |
| Backup capability | Whether and how data is backed up/restored (none, volume snapshot, engine-specific); verified restorability stays a distinct state. |
| Capability flags | Stateful or stateless, local-storage identity needs, exposed endpoints, required secrets, supported platforms. |

Engine-specific safety logic (Galera quorum, bootstrap, guided full-cluster recovery, A02/A06) stays explicit code behind the service-type interface. The abstraction must not water down these rules; a generic "restart until healthy" loop is not acceptable for stateful types.

### Platform back ends (orchestrators)

Hide the target platform behind an orchestrator interface: deploy/update/remove workloads, secrets, networks, volumes with host identity, placement constraints, observation and events. Swarm is the first back end (current work). Kubernetes is a later back end; it must not inherit Swarm or Docker assumptions.

*Cluster provisioning* is a separate capability from managing workloads on an existing cluster: bootstrapping Docker/Swarm or a Kubernetes distribution on prepared hosts over SSH and/or the manager agent, joining/removing nodes and showing the exact commands. It has its own threat model (SSH credentials, host privileges) and is never applied to hosts the user did not explicitly register for provisioning.

### Image build service and registries

- A build service based on BuildKit (proposed; alternatives are an open decision) turns *build recipes* into images. A recipe is generated from UI selections (for example Nginx version, PHP version, PHP extension list, `php.ini` values, Nginx configuration files) and is stored as reviewable data.
- Builds should be reproducible: pinned base image digests, pinned package versions where the distribution allows, recorded build inputs, and multi-architecture output (AMD64/ARM64, see P10).
- Each build produces an SBOM, can call a vulnerability-scan hook and can optionally sign the image. Scan results inform the user; blocking policy is configurable.
- A registry abstraction supports either a manager-operated registry service (itself a service type) or an external registry such as ghcr. Registry credentials are stored as secrets, never in recipes, logs or exports.
- Distribution rules define which images go to which registry and which platforms/hosts may pull them, including offline/air-gapped operation with a managed registry.

### Explainability ("show what will happen")

Every UI action that changes state shows the resulting plan before execution: steps, affected resources, the generated configuration (for example Swarm/Kubernetes YAML, Traefik routes, build recipe/Dockerfile) and the equivalent commands or API calls. Users can export the desired state as code and open a read-only expert view of the generated artefacts and observed state. The UI is the primary interface, not a black box; the audit log (U07) records what was actually executed.

### Docker engine updates

Proposed workflow, subject to the open boundary decision: per-host rolling updates, one host at a time; drain the host using A08 maintenance checks (quorum and remaining healthy nodes for stateful types); check the target version against a maintained compatibility matrix (Docker engine, Swarm/Kubernetes version, OS, architecture); update; verify health; then continue. Rollback is limited: package downgrades may be unsupported by the distribution and some daemon/state changes are not reversible, so the plan must state rollback limits before approval. Host restarts are not performed by the manager unless the owner explicitly opts in through the open decision.

## Alternatives considered

- **Keep the product database-only** and build a separate tool for other services. Rejected by the owner's direction.
- **Generic container management first, Galera later.** Rejected: the owner set Galera as priority 1, and the safety-critical stateful case should shape the abstractions.
- **Hard-code each new server type** without a service-type definition. Rejected as a principle: it would duplicate UI, planning and update logic and make transparency inconsistent. The concrete definition format (Go code plus JSON Schema, or a data-only format) is open.
- **Wrap an existing PaaS (for example Portainer, Coolify, Rancher) instead of building.** Not evaluated yet; recording it here so later research can compare functionality, licensing (AGPLv3 compatibility) and the explainability requirement.

## Consequences

- The project name "DB Cluster Manager"/"Galera Cluster Manager" no longer describes the product; naming is an open decision.
- R1 work proceeds unchanged. New phases are proposed after the existing release plan; their order relative to R2–R5 is open.
- Interfaces introduced during R1 (engine capabilities, orchestration platform, backup transport) should be shaped so that Galera can later be expressed as a service type without rewriting its safety logic. This is guidance, not a new R1 gate.
- New threat surfaces: image builds (untrusted build inputs, build-time network access), registry credentials, SSH-based host provisioning, Traefik configuration exposure, and engine updates on hosts. Each needs a security review before implementation.
- Bundled third-party images (Nginx, PHP, Traefik, registry, MinIO-like servers) keep their own licenses; the license implications are unchanged and must be documented per type.
- Local test budget (U11) applies to all new phases; image builds and Kubernetes clusters are resource-heavy and must run sequentially within it.

See [requirements](../KONZEPT.md#scope-extension), [release plan](../RELEASES.md), [architecture](../ARCHITEKTUR.md#extension-boundaries) and [open decisions](../ENTSCHEIDUNGEN.md).
