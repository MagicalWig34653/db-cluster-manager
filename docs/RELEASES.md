# Proposed release plan

Status: staged delivery approved; the allocation below is a proposal for review. Phase labels are planning labels, not published versions or deadlines. Every confirmed requirement remains in scope. Nothing here is implemented.

## P0 — Local feasibility prototypes

Build isolated Colima/Lima experiments within [the shared resource budget](TESTING.md), before using the user's Swarm.

Scope: Swarm service deployment by a manager, one agent per host, three local-storage Galera nodes, competing-controller prevention, failure replacement and returning-node behavior. Separately prove replicated manager state/fencing without shared storage and evaluate redundant proxy routing/VIP possibilities. Test supported architectures with evidence appropriate to each.

Exit gates:

- Healthy cluster creation and read/write smoke checks on dedicated VMs.
- Single-host loss/replacement, old-host return and network partition do not create unsafe identities or uncontrolled bootstrap.
- Manager leader loss does not duplicate an operation; database serving continues.
- Unrelated sentinel workloads remain unchanged.
- Resource use measured and scoped teardown removes only owned test resources.
- Record architecture decisions, observed limitations and reproducible commands.

Periodic standby refresh and migration port handover can use separate short experiments; unresolved optional features must be explicitly deferred rather than assumed proven.

## R1 — First working version for the user's Swarm trial

A small-installation baseline: Swarm connection, automatically deployed agents, new MariaDB Galera clusters, local storage, single/HA managers, multi-Swarm resource model, placement rules, spare-host replacement, guided full-outage recovery, host maintenance and resource reservations. Redundant database endpoint and direct node access, basic configurable writer routing, client-facing TLS with imported/internal certificates, Docker compatibility checks, AMD64/ARM64 and optional offline installation.

Include local passkey/passkey-only authentication, recovery mechanisms, resource/action permissions, basic database/user/settings management, drift handling, operation/audit history, native health dashboards, UI/email/webhook notifications, metric retention limits, cluster backups to S3/SFTP and verified full restore. Manager configuration backup/setup recovery starts in inspection mode. German/English UI and user guides; English technical documentation.

Default updates are manual, using curated compatible paths and explicit major-version approval. Implement DB-only LTS/Rolling channel metadata. Advanced automation follows R2.

Exit gates:

- Fresh install and configuration restore documented and reproduced locally.
- End-to-end cluster deployment, host failure, manager failure and restore tested with preserved data.
- Permissions, secret handling and protected resource ownership verified.
- Partial operations and restart recovery behave deterministically.
- Working version packaged with compatibility matrix, limitations, recovery guide and teardown instructions.
- Only then invite the user to test on their existing Swarm; this is not a declaration of production readiness.

## R2 — Operations, migration and richer UI

Standalone in-place MariaDB migration on Swarm hosts, minimized write interruption, verified copy of data/accounts and approved address/port cutover. Database content/user/permission import/export and integrated phpMyAdmin with both login modes. Expand settings coverage and safe per-node overrides.

Add continuously synchronized standbys, configurable latency-aware proxy routing and optional read/write splitting, ACME DNS-01, optional DNS-provider management, optional encryption of remaining database links, and supported VIP configurations where proven. Optional scheduled DB updates with major upgrades still explicitly approved.

Comprehensive statistics/query analysis, personally configurable and shared dashboards/templates. Per-database backup policy overrides and optional scheduled restore verification.

Exit gates: migration and failed-cutover scenarios, routing/transaction semantics, grant enforcement, certificate renewal and update failure paths tested. Query overhead and data handling documented.

## R3 — Advanced recovery and historical data

Optional PITR; open backups in isolated temporary databases, inspect/export and selectively restore tables/databases with preview/approval. Configurable restore placement, resource limits and expiry cleanup. S3/SFTP long-term metrics archive with direct dashboard retrieval and bounded cache.

Exit gates: known-time recovery with verified data, selective restore conflict handling, abandoned-session cleanup after manager restart, and historical queries with unavailable/corrupted archive objects tested. No restored copy may rejoin production or restart production replication accidentally.

## R4 — Independent-cluster copies and passive reserves

Single-source replication to independent clusters, all/selected databases, database-specific read access, lag reporting and no automatic source promotion. Periodically refreshed passive reserve only after feasibility evidence; document when full synchronization is unavoidable.

Exit gates: filtering/DDL/privilege tests, source outage without promotion, interruption/catch-up and old-copy safety validated. If periodic refresh is infeasible, return a concrete limitation and alternatives for user decision.

## R5 and later — Platform and engine expansion

Adoption of existing full clusters, Percona XtraDB and compatible Galera-to-XtraDB migration, PostgreSQL/other engines, and native Kubernetes orchestration. Ordering within this phase remains open. Each addition needs its own compatibility matrix, operations/recovery design and end-to-end tests.

## Scope extension phases (proposal)

Added 2026-10-22 after the owner broadened the scope ([ADR 0012](decisions/0012-broadened-scope-platform.md), requirements X01–X12). **Not started: no code, experiment or test exists for any of these phases.** They are proposals for review, not commitments; no allocation implies a date.

R1 (MariaDB Galera on Swarm) stays first and unchanged (X11). P0 and R1 gates are not relaxed, and extension work does not start before R1 unless the owner decides otherwise. The order of R6–R9 relative to R2–R5 is an open decision; the numbering only avoids renumbering the existing plan. Native Kubernetes from R5 is planned in detail in R9.

Local-test notes for all phases: the U11 budget (16 GB RAM, 50% CPU, ~100 GB SSD) applies to the sum of all running VMs, builds and registries. Image builds, registries and Kubernetes nodes are disk- and memory-heavy; run their suites sequentially, cap BuildKit cache size, and include them in the lab ownership inventory and scoped cleanup. External registries (for example ghcr) are tested only with dedicated test namespaces and credentials, never production ones.

### R6 — Service type framework, generic containers and Docker engine updates

Service-type definition format (desired-state schema, UI form schema, planner, reconcile/health, update strategy, backup capability), express Galera through it without changing its safety logic, generic container/service type, plan preview and export-as-code for every type, read-only expert view. Docker engine update workflow within the boundaries the owner confirms for X09 (rolling, one host at a time, drain with A08 checks, compatibility matrix, stated rollback limits).

Exit gates:

- Galera behaviour and R1 regression suite unchanged after moving it behind the service-type interface.
- Generic container create/update/rollback/remove on a lab Swarm; unrelated sentinel workloads unchanged (A10).
- Every mutating UI action shows a plan whose executed steps match the audit log.
- Engine update on a 3-host lab Swarm with a running Galera cluster: one host at a time, quorum preserved, failure mid-update stops the rollout; downgrade limits documented.

### R7 — Image build service, registries and web server composer

BuildKit-based build service (pending the build-tool decision), recipes generated from UI selections, reproducible builds with pinned inputs, multi-architecture output, SBOM, vulnerability-scan hook, optional signing. Registry abstraction: manager-operated registry service and external registries with credentials as secrets; distribution rules. Web server type with composer: Nginx version, PHP version, PHP extensions and `php.ini`, Nginx configuration with validation (`nginx -t` in the build/preflight), rolling updates.

Exit gates:

- Same recipe and pinned inputs produce an equivalent image (documented reproducibility limits); SBOM and scan report stored per build.
- Push/pull through the managed registry and one external test registry; credentials never appear in recipes, logs, exports or audit records.
- Invalid Nginx/PHP configuration is rejected before deployment; a failed rollout leaves the previous version serving.
- Build resource limits and cache cleanup verified within the lab budget.

### R8 — Storage protocol servers and Traefik

Storage server types: SFTP, FTP(S), S3-compatible object storage, NFS (feasibility on Swarm with local storage to be verified), further protocols by request. Users/credentials, quotas where supported, TLS, backup capability per type. Traefik type: deployment, UI route editor for routers/services/middlewares, certificate integration with N05, plan preview of generated dynamic configuration.

Exit gates:

- Each storage type: create, client access smoke test, credential rotation, update and backup/restore (where declared) on the lab.
- NFS limitations under Swarm (privileges, kernel modules, host networking) documented with evidence, or the type is deferred.
- Traefik route changes are previewed, applied without dropping unrelated routes and reverted on failure; existing user-managed Traefik instances are never modified without adoption.

### R9 — Kubernetes back end and cluster provisioning

Kubernetes orchestrator back end for service types (starting with stateless types; Galera on Kubernetes needs its own storage-identity and recovery design). Cluster provisioning: bootstrap Swarm and a chosen Kubernetes distribution on registered hosts over SSH and/or the agent, join/remove nodes, show all commands.

Exit gates:

- Provisioning a Swarm and a small Kubernetes cluster on lab VMs is reproducible and fully removable by scoped cleanup.
- Service types declare and enforce their supported platforms; no Swarm/Docker assumption leaks into the Kubernetes back end.
- SSH credentials and provisioning privileges reviewed; provisioning never touches unregistered hosts.

### R10 — AI-assisted vulnerability response (proposal, X13)

Proposed 2026-10-23 ([ADR 0015](decisions/0015-ai-assisted-vulnerability-response.md)); **not started, no code or test exists.** Depends on R6 (service types) and R7 (build service, registry, SBOM, scan hook). Deterministic part: SBOM-backed inventory, advisory feed polling (NVD, OSV, distribution/vendor and upstream feeds, offline mirror), matching, findings with VEX-like status, notifications. AI part (optional): provider adapters (Claude Code login, Claude API, Codex login, OpenAI API, OpenAI-compatible endpoints such as LM Studio/Ollama) with selectable models, redaction and egress consent, structured proposals validated against typed models and allow-lists, digest-bound approval, optional opt-in auto-apply with canary, health gate and rollback (never for databases, major upgrades or Galera quorum rules), mitigation tracking with expiry, audit of prompt hashes, model, proposal and decision.

Exit gates:

- Matcher precision/recall on a corpus of past advisories; false positives handled by VEX-like status.
- Injection corpus: no proposal outside the allow-list is accepted; the AI has no tool access to the cluster.
- Redaction corpus: no secret pattern leaves the manager; per-provider consent enforced; local-only operation works without network egress.
- Approved patched rebuild rolled out on a lab Swarm with canary and rollback on a deliberately failing image; auto-apply refused for database services.
- Mitigations expire and re-alert; the audit log lets an admin reconstruct each decision.

## Requirement coverage

| Requirements | Earliest planned delivery / expansion |
| --- | --- |
| P01–P07, P09–P10 | P0 validation; R1 baseline |
| P08 | MariaDB/new clusters R1; other engines/adoption/Kubernetes R5+ |
| A01–A02, A04–A10 | R1; scaling advice enhanced with R2 telemetry |
| A03 | Spare hosts R1; continuous standby R2; periodic reserve R4 |
| D01, D03 | Basic support R1; expanded coverage R2 |
| D02, D04–D06 | R2 |
| D07 | R5+ |
| D08–D09 | Manual supported paths/channels R1; optional automatic updates R2 |
| N01–N02 | R4 |
| N03–N04 | Redundant endpoint/direct access/basic routing R1; latency/splitting/VIP R2 after validation |
| N05–N07 | Imported/internal endpoint certificates R1; ACME/DNS and optional full database TLS R2 |
| B01 | Cluster scope R1; database overrides R2 |
| B02–B05 | R3 |
| B06 | R2 |
| B07–B09 | R1 |
| U01–U02 | R1; phpMyAdmin enforcement R2 |
| U03–U04 | Native health baseline R1; rich configurable dashboards/query analysis R2 |
| U05 | Local retention/limits R1; archive retrieval R3 |
| U06–U09 | R1 and continuously thereafter |
| U10 | Concept completion before implementation; documentation handover maintained throughout |
| U11 | P0 and every local test stage |
| X01, X03, X06, X12 | R6 (framework, transparency, generic containers); extended in every later phase |
| X02 | Kubernetes back end and cluster provisioning R9 |
| X04, X08 | R7 |
| X05, X07 | R8 |
| X09 | R6, after the owner confirms the update boundaries |
| X10 | Generic containers R6; per-type update strategies R7–R8 |
| X11 | P0 and R1 unchanged; constrains R6–R10 ordering |
| X13 | R10 (proposal; after R6 and R7) |

R1 is still substantial. Re-slice its internal work packages after P0 rather than skipping its safety/recovery gates. Run test suites sequentially when simultaneous topology sizes would exceed the Mac budget.
