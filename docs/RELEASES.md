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

R1 is still substantial. Re-slice its internal work packages after P0 rather than skipping its safety/recovery gates. Run test suites sequentially when simultaneous topology sizes would exceed the Mac budget.
