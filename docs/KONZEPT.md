# Confirmed product requirements

Updated: 2026-10-22 (scope extension added). These requirements record user choices, not implemented functionality. Release allocation is in [RELEASES.md](RELEASES.md); unresolved implementation choices are in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md).

## Product and deployment

| ID | Confirmed requirement |
| --- | --- |
| P01 | Free, self-hosted, open source under AGPLv3; inspired by ClusterControl. |
| P02 | Svelte with shadcn-svelte; visual reference is `timetrack-pwa`. Go is selected for manager, agent and CLI. Use a monorepo with separate frontend source and a combined manager/UI distribution. |
| P03 | Deploy databases directly inside an existing Docker Swarm. Kubernetes support comes later. *Note 2026-10-22: extended by [X01–X02](#scope-extension) (platform choice and cluster provisioning); Swarm-first for Galera is unchanged.* |
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
| A09 | Host restarts and Docker upgrades remain the user's responsibility. Show Docker compatibility warnings and recommended versions against a maintained matrix. **Partly superseded 2026-10-22 by [X09](#scope-extension):** Docker engine updates are now wanted in scope; the boundary for host restarts and automatic vs approved steps is an open decision. The compatibility-matrix part remains valid. |
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

## Scope extension

Confirmed by the owner on 2026-10-22 as product direction ([ADR 0012](decisions/0012-broadened-scope-platform.md)). These rows record wishes, not implemented functionality; mechanisms are proposals in [ARCHITEKTUR.md](ARCHITEKTUR.md#extension-boundaries) and open choices are in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md).

| ID | Confirmed requirement |
| --- | --- |
| X01 | Broaden the product from a database cluster manager to a general self-hosted platform for clusters and services. Database clusters remain a core use case. |
| X02 | Platform choice per managed environment: Docker Swarm or Kubernetes. Optionally build (provision) new Swarm or Kubernetes clusters on user-provided hosts and manage them afterwards, in addition to connecting existing clusters. |
| X03 | Operation is as intuitive as possible and possible 100% through the UI, without hiding the system: users must still be able to understand what is created and changed (for example plan preview, generated configuration, equivalent commands, export as code). |
| X04 | Web servers as a managed server type, including detailed composition: choose e.g. Nginx version, PHP version, specific PHP extensions/functions and `php.ini` settings, and provide or edit the Nginx configuration. Other web server software may follow. |
| X05 | Storage protocol servers as managed server types: SFTP, FTP(S), S3-compatible object storage, NFS and further protocols where feasible. |
| X06 | Generic Docker containers/services as a managed type, for workloads without a dedicated type. |
| X07 | Optionally manage Traefik directly through the UI (deployment, routers/services/middlewares, certificates). Exact scope is a proposal to be refined. |
| X08 | The manager builds images itself from UI-composed configurations (such as X04) and uploads/distributes them to either a container registry optionally operated by the manager or a configured external registry (for example ghcr). |
| X09 | Include Docker engine updates on managed hosts. This partly supersedes A09; who restarts hosts and which steps are automatic or require approval must be confirmed separately. |
| X10 | Updates of managed services (images and versions of web servers, storage servers, generic containers, Traefik, registry) are supported through the UI, following the update rules of each type. |
| X11 | MariaDB Galera has priority 1. The extension must not delay or weaken R1 scope and gates. |
| X12 | Keep the user able to manage multiple platforms and service types side by side, with existing safety principles (A10 ownership, U02 permissions, U07 audit, no secrets in exports) applying to all new types. |
| X13 | AI-assisted response to critical vulnerabilities (proposed by the owner 2026-10-23): the manager regularly polls advisory feeds (NVD, OSV, distribution/vendor and upstream project feeds), matches them against the inventory of images, packages and versions it manages, works out a fix (optionally with AI analysis), rates benefit and risk, rebuilds the image with a patched upstream version if one exists or otherwise proposes a mitigation that reduces the attack surface (for example disabling PHP upload progress or changing a configuration option), builds and distributes a new image through the image build service and registry (X08), and informs the admin. The AI part is optional with selectable providers (Claude Code login, Claude API, Codex login, OpenAI API, generic OpenAI-compatible APIs such as LM Studio or Ollama) and a selectable model per provider. Safety constraints are proposed in [ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md): AI output is advisory, changes need admin approval unless an explicit per-severity and per-service auto-apply policy applies, and databases, major upgrades and Galera quorum rules are never auto-applied. |
| X15 | General OS patch management for servers (owner 2026-10-27, modelled on PatchMon): a lightweight agent reports installed packages, available updates, security updates, reboot-required and OS/kernel information per host; the UI offers per-host and per-package views, host groups and an update history. The owner explicitly approves an agent installed directly in the OS (native, systemd service) where necessary, in addition to the Docker-based agent ([ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md)). Default is report-only; changes need an approved plan with maintenance windows and per-host/group policies (security-only option, dry run/simulation). A reboot never happens automatically unless the policy says so. A Galera member is never patched or rebooted unless the cluster planner's rolling procedure coordinates it. Installed OS packages feed the vulnerability inventory (X13). Mechanism: [ADR 0023](decisions/0023-os-patch-management.md); the UI is not built yet. |

Note: X12 restates that existing cross-cutting requirements apply to new types; it adds no new mechanism.
