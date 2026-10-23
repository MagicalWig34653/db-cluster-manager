# Decisions and remaining questions

Updated: 2026-10-22 (scope extension). [Requirements](KONZEPT.md) are the canonical detailed record of confirmed user choices. [ADR 0001](decisions/0001-product-direction.md) records the resulting product direction.

## Confirmed direction

Swarm-native deployment first; manager-deployed agent services; multiple Swarms; local storage; manager HA with at least three instances; MariaDB Galera first; small installations; staged releases; AGPLv3; English technical documentation/code and bilingual UI/user guides. The earlier external-Docker-first suggestion is superseded.

Automatic single-node replacement is allowed by policy/capacity. Full-cluster recovery and major upgrades require explicit approval. Cross-cluster copies have one source and no automatic promotion. All detailed defaults are recorded in the requirements rather than duplicated here.

**Scope broadened (2026-10-22, [ADR 0012](decisions/0012-broadened-scope-platform.md)):** the product becomes a general self-hosted platform for clusters and services: Swarm or Kubernetes (including provisioning new clusters), additional server types (web servers with detailed composition, storage protocol servers, generic containers, optionally Traefik), image building with a managed or external registry, and Docker engine updates — fully operable through the UI while keeping the system understandable. MariaDB Galera remains priority 1; R1 scope and gates are unchanged. Requirements: X01–X12 in [KONZEPT.md](KONZEPT.md#scope-extension). A09 is partly superseded by X09.

**Technical decisions of 2026-10-23 (proposed, fakes-only evidence):** data-key distribution through a replicated keyring with per-replica and recovery-key envelopes, administrator registration of replicas, metastore peer mTLS and certificate deny-list distribution to the agents ([ADR 0013](decisions/0013-replica-keys-peer-tls-and-revocation.md)); keyed backup chunk digests in manifest format 2 ([ADR 0009 amendment](decisions/0009-cluster-backup-method.md)). Approving a guided full-outage recovery needs the new `recover` permission (administrator preset only). Host maintenance never reboots or drains a host and does not change its Swarm availability (consistent with the open X09 question below).

## Remaining product decisions

- Final project name/GitHub owner, release naming and Pages setup. Public visibility and Git initialization/public push by Claude are explicitly authorized.
- Copyright identity (the license is decided: `AGPL-3.0-only`).
- Choose initial supported database/Docker versions and LTS/Rolling mapping. Go and the monorepo are confirmed; see ADR 0002.
- Numerical recovery objectives (RPO/RTO), failure detection/replacement delays, retention/storage defaults beyond metrics.
- Initial DNS provider integrations; email delivery setup and webhook authentication.
- Exact host selection precedence, available-capacity definition and admission/eviction behavior.
- Query collection default, SQL redaction and query-log retention.
- Credential export/import behavior and separate phpMyAdmin-login policy.
- Confirm the proposed release allocation after feasibility work; staged releases themselves are approved.

## Open decisions from the scope extension

None of these is decided. Ask the owner one focused question at a time when the dependent work becomes current; R1 work does not depend on them.

- **Docker engine update boundaries (X09, supersedes A09 in part).** Who restarts hosts (always the user, or the manager after opt-in)? Which steps may run automatically (for example in maintenance windows) and which always need approval? Are OS package updates beyond the Docker engine in scope? Which installation methods are supported (distribution packages, Docker's repository, others)? Until confirmed, the manager performs no host restarts and no unattended engine updates.
- **Product name.** "DB Cluster Manager"/"Galera Cluster Manager" no longer fits. Options, without a choice: keep the name as a legacy repository name and pick a new product name; a neutral platform name (for example "Swarmhold", "Clusterwerk", "Fleetkeeper" — availability and trademark not checked); a descriptive name ("Self-hosted Cluster Manager"). Also decide whether the GitHub repository is renamed (redirects) and when.
- **Kubernetes timing.** Whether Kubernetes (X02) stays after the service-type, build and storage phases (proposed R9) or moves earlier; which distributions to support first (for example k3s, kubeadm, existing managed clusters).
- **Managed registry implementation.** For example CNCF Distribution (`registry`), Harbor, Zot or another; criteria: resource use within the lab budget, multi-architecture, garbage collection, authentication, replication to external registries, license.
- **Build tool.** BuildKit (proposed) vs alternatives such as Buildah/Podman, Kaniko or ko-style builders; rootless operation, build host placement, cache limits, multi-architecture (emulation vs native builders).
- **Web server composition scope (X04).** Supported web servers (Nginx only first, or also Apache/Caddy), PHP distribution source (official images vs distribution packages), curated extension list vs arbitrary PECL/source builds, config editing depth (templates vs free-form files), multiple sites per instance.
- **Storage server scope (X05).** Which protocols first; NFS feasibility under Swarm; whether S3-compatible storage is a managed third-party server (license check required) or an external integration.
- **Traefik scope (X07).** Manager-deployed Traefik only, or adoption of an existing user-managed Traefik; overlap with the R1/R2 database endpoint proxies.
- **Order of R6–R9 relative to R2–R5** in [RELEASES.md](RELEASES.md#scope-extension-phases-proposal).
- **License implications of bundled third-party images** are unchanged in principle: each image and recipe keeps its upstream license; document per type, and do not redistribute images whose terms forbid it.

## Technical decisions requiring evidence

| Topic | Required output |
| --- | --- |
| Manager HA without shared storage | Replicated metadata/secret store, coordination/fencing, recovery and membership ADR |
| Swarm ownership and node-local data | Manager/agent/scheduler responsibility and returning-host behavior ADR |
| Endpoint HA | Proxy selection, DNS/VIP feasibility, client failover and latency-routing evidence |
| Warm and periodic reserves | Membership semantics, refresh strategy, catch-up thresholds and full-transfer fallback |
| Backups and PITR | Consistent backup/log positions, version support, tested restore pipeline and retention coupling |
| Cross-cluster copies | Supported source/target matrix, filters, account/DDL behavior and lag handling |
| Standalone migration | Compatibility preflight, port handover, cutover checks and limits of rollback |
| Manager recovery | Key recovery, old-manager fencing, passkey origin and live-state reconciliation |
| Telemetry | Embedded storage/aggregation, archive index, bounded cache and access control |
| Multi-architecture | Native ARM64 and actual AMD64 validation strategy within local limits |
| Service-type definition | Format (Go code plus schema vs data-only), versioning, how Galera's explicit safety logic is expressed, UI schema generation |
| Image builds and supply chain | Reproducibility limits, SBOM format, scan tool integration, signing (for example cosign), build isolation and cache budget |
| Kubernetes back end and provisioning | Storage identity for stateful types, SSH/agent bootstrap security, removal and cleanup |

Ask one focused question at a time and wait for the user's response. Do not ask a new question while continuing decisions dependent on its answer. Technical experiments may resolve implementation choices without turning them into unnecessary product questions.
