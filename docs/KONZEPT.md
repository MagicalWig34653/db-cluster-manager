# Confirmed product requirements

Updated: 2026-10-18. These requirements record user choices, not implemented functionality. Release allocation is in [RELEASES.md](RELEASES.md); unresolved implementation choices are in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md).

## Product and deployment

| ID | Confirmed requirement |
| --- | --- |
| P01 | Free, self-hosted, open source under AGPLv3; inspired by ClusterControl. |
| P02 | Svelte with shadcn-svelte; visual reference is `timetrack-pwa`. Go is selected for manager, agent and CLI. Use a monorepo with separate frontend source and a combined manager/UI distribution. |
| P03 | Deploy databases directly inside an existing Docker Swarm. Kubernetes support comes later. |
| P04 | The user deploys only the manager, through Docker Compose or as a Swarm service, inside or outside the target Swarm. External deployment connects to its Docker API; secure connection details remain to be designed. |
| P05 | The manager deploys agents as services, with one agent per managed Docker host. The user does not deploy agents manually. |
| P06 | Manage multiple independent Swarms and multiple database clusters. |
| P07 | Support local storage without requiring distributed storage. Manager HA uses at least three instances; a single instance is available for small test installations. |
| P08 | Start with MariaDB Galera and new clusters. Percona XtraDB Cluster, PostgreSQL, other engines, Kubernetes, and adoption of existing complete clusters follow later. |
| P09 | Target small installations first. Three or five Galera nodes are initial reference topologies, not an asserted engine maximum. |
| P10 | Support AMD64 and ARM64 using verified images and tools. Internet egress is the default; manager UI/API are internal by default. Offline operation is optional with available images and dependencies. |

## Placement, automation and host protection

| ID | Confirmed requirement |
| --- | --- |
| A01 | Placement modes: automatic selection of suitable hosts, host exclusions, explicit host preferences/allowances, and strictly allowlisted hosts. Exact precedence must be defined. |
| A02 | Automatically replace failed database nodes when sufficient capacity exists, including configured spare hosts. |
| A03 | Support spare hosts and continuously synchronized standby nodes without application traffic. Also investigate a periodically refreshed passive reserve to reduce later catch-up work; feasibility is unproven. |
| A04 | Propose CPU/RAM limits and let users adjust them, including a configurable reserve for unrelated host workloads. |
| A05 | Suggest scale-out for sustained load; do not automatically scale for load initially. Automatic failure replacement is separate. |
| A06 | Full-cluster failure uses guided recovery with diagnosis and explicit approval. Never treat it as ordinary single-node replacement. |
| A07 | Show drift, let users adopt it as desired state or revert it; automatic reversion is optional. |
| A08 | Guided host maintenance checks remaining healthy nodes, redirects traffic, pauses replacement for the host, and indicates readiness for maintenance. |
| A09 | Host restarts and Docker upgrades remain the user's responsibility. Show Docker compatibility warnings and recommended versions against a maintained matrix. |
| A10 | Do not modify unrelated containers/resources. Management must constrain its own resource consumption; shared hardware cannot promise complete performance isolation. |

## Database operations and migration

| ID | Confirmed requirement |
| --- | --- |
| D01 | Manage database creation, users and fine-grained database permissions. Offer a generated user/password when creating a database; allow later changes in the UI. |
| D02 | Import/export database contents and users/permissions. Credential portability and secure export format remain to be specified. |
| D03 | Expose database server settings such as `max_connections`, cluster-wide with per-node overrides where safe. Distinguish dynamic settings, restart requirements and unsupported settings. Aim for comprehensive coverage. |
| D04 | Embed phpMyAdmin for MariaDB/Galera. Automatic login with appropriate database privileges is the default; separate database-credential login is also available. |
| D05 | Migrate a standalone MariaDB container on a Swarm member host into a managed cluster: copy data, accounts and permissions, verify, then replace the original container with a managed node. |
| D06 | Migration uses a guided maintenance window. Prepare work beforehand where feasible, estimate duration, minimize write interruption and request explicit cutover approval. Preserve address/port where possible; no fixed duration guarantee for arbitrary data sizes. |
| D07 | Later support migration from MariaDB Galera to Percona XtraDB where compatible. No general cross-engine compatibility promise. |
| D08 | Updates are manual by default, optionally automatic in maintenance windows. Major version changes always require explicit approval. |
| D09 | Select Rolling or LTS channels per database cluster. These channels apply only to database versions, not manager/agent releases. Supported upgrade paths must be curated. |

## Cross-cluster copies and routing

| ID | Confirmed requirement |
| --- | --- |
| N01 | Replicate from one leading source cluster to independent target clusters, either all databases or selected databases. |
| N02 | Targets are warm copies, optionally readable. No automatic source promotion or failover is in the initial scope. Enable read access independently per replicated database. |
| N03 | Provide an HA database endpoint with proxies on multiple hosts. Each proxy considers its own latency to database nodes and the configured routing policy. Applications use a domain or optional virtual IP. Direct node access remains possible. |
| N04 | Configurable write routing, including a preferred writer or multiple writers. Read/write splitting is optional. Exact consistency and transaction rules require design. |
| N05 | Make TLS simple: ACME DNS-01, imported certificates, and a manager-operated internal CA. Automate renewal. |
| N06 | Default database TLS scope is application-to-endpoint. Encryption endpoint-to-node, for direct node access, and between Galera nodes is optional. Manager/agent transport security is a separate requirement. |
| N07 | Optional automatic DNS creation/update through provider integrations; manual DNS records also work. HA virtual IP feasibility and network prerequisites must be tested. |

## Backups and recovery

| ID | Confirmed requirement |
| --- | --- |
| B01 | S3/S3-compatible and SFTP backup destinations. Global defaults with overrides per cluster/database for schedule, retention and destinations. |
| B02 | Optional point-in-time recovery using appropriate continuously archived change logs. |
| B03 | Open any supported backup by restoring to a temporary isolated database and providing browsing access, for example via phpMyAdmin. Remove the environment on completion; expiry also cleans abandoned sessions. |
| B04 | Browse/export and selectively restore individual databases or tables from an opened backup. Preview and explicit confirmation precede writes to existing targets. |
| B05 | Temporary restores use either explicitly approved hosts or automatic suitable-host selection. Resource limits and cleanup apply in both modes. |
| B06 | Optional scheduled restore verification with its own schedule and resource limit. |
| B07 | Back up manager configuration to S3/SFTP, including cluster mappings, users, permissions and encrypted credentials. Use an externally retained recovery key. |
| B08 | Initial setup offers new installation or recovery by uploading a configuration backup or connecting directly to S3/SFTP and selecting a backup. |
| B09 | A restored manager first inspects live resources and reports differences. Management changes require explicit confirmation before resuming. |

Warm copies and synchronized standby nodes do not replace retained backups: erroneous changes may propagate. Backup success and verified restorability are distinct states.

## Access, observability and documentation

| ID | Confirmed requirement |
| --- | --- |
| U01 | Local users first; passkeys are primary and passkey-only accounts are supported. Recovery options: recovery codes, an optional password, or a short-lived single-use recovery URL generated through the manager CLI. |
| U02 | Fine-grained resource/action permissions down to individual databases, with preset roles and customization. phpMyAdmin access is separately permissioned and must enforce appropriate database privileges. |
| U03 | Comprehensive host/database/replication/backup/query statistics in the product UI. Collect/manage the monitoring infrastructure automatically; no Grafana dependency. |
| U04 | User-configurable dashboards with movable widgets/charts/tables, personal dashboards, shared dashboards and reusable templates. Resource permissions apply to displayed data. |
| U05 | Default metric retention: detailed values for 7 days, aggregated values for 90 days. Both configurable with a storage limit. Optional S3/SFTP long-term archive, queried directly from dashboards through on-demand retrieval and local caching. |
| U06 | UI notifications, email and configurable webhooks for failures and required actions. |
| U07 | Audit all management actions with actor/automation rule, time, changes and result; configurable retention/export and no secrets. |
| U08 | UI in German and English, selectable per user. Code, identifiers and code comments entirely English. Technical documentation initially English; user guides German and English. |
| U09 | Continuously maintained `docs/` and GitHub Pages from the same source. Repository is the source of truth; sessions are disposable. |
| U10 | At the end of concept work, prepare a Claude Code environment to take over development. Split implementation into releases. |
| U11 | Prototypes run locally on the user's M4 Pro Mac (48 GB physical RAM), using Colima and dedicated Lima VMs. Combined budget: 16 GB RAM, 50% CPU capacity, approximately 100 GB SSD. All test resources must be selectively removable. The user's existing Swarm is used only after the first working version. |
