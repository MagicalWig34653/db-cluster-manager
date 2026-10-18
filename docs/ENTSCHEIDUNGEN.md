# Decisions and remaining questions

Updated: 2026-10-18. [Requirements](KONZEPT.md) are the canonical detailed record of confirmed user choices. [ADR 0001](decisions/0001-product-direction.md) records the resulting product direction.

## Confirmed direction

Swarm-native deployment first; manager-deployed agent services; multiple Swarms; local storage; manager HA with at least three instances; MariaDB Galera first; small installations; staged releases; AGPLv3; English technical documentation/code and bilingual UI/user guides. The earlier external-Docker-first suggestion is superseded.

Automatic single-node replacement is allowed by policy/capacity. Full-cluster recovery and major upgrades require explicit approval. Cross-cluster copies have one source and no automatic promotion. All detailed defaults are recorded in the requirements rather than duplicated here.

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

Ask one focused question at a time and wait for the user's response. Do not ask a new question while continuing decisions dependent on its answer. Technical experiments may resolve implementation choices without turning them into unnecessary product questions.
