---
title: User guides (English)
---

# User guides (English)

These guides describe what the software does **today**. This is pre-release software: nothing has run on a production Swarm, and the first Swarm trial has not started. Every feature carries a verification label taken from [STATUS.md](../../STATUS.md) and the lab results in [tests/p0/results](../../../tests/p0/results/e4-manager-galera.md).

German version: [Benutzerhandbuch (Deutsch)](../de/index.md). Technical details: [INSTALL.md](../../INSTALL.md), [KONZEPT.md](../../KONZEPT.md), [api/openapi.yaml](../../../api/openapi.yaml).

## Verification labels

| Label | Meaning |
| --- | --- |
| **[Lab VMs]** | Ran with the real manager, real mTLS agents and real MariaDB 11.4 on dedicated Lima VMs (arm64), observed once or a few times (experiments E1-E6). Not a production proof. |
| **[Fakes]** | Implemented and unit-tested only against fakes (fake Docker, fake agents, in-memory stores, `httptest`). It has never run against the real thing. Treat as untrusted until a lab run exists. |
| **[Untested]** | Documented design or code path with no test of the real behaviour at all, or not implemented. |

Where one feature has parts at different levels, each part is labelled separately.

## Guides

1. [Getting started and installation](01-getting-started.md)
2. [Enrolling hosts and agents](02-hosts-and-agents.md)
3. [Creating a Galera cluster](03-create-cluster.md)
4. [Health and alerts](04-health-alerts.md)
5. [Backups and restore](05-backup-restore.md)
6. [Node replacement](06-node-replacement.md)
7. [Full-outage recovery with approval](07-full-outage-recovery.md)
8. [Security model](08-security-model.md)
9. [Troubleshooting](09-troubleshooting.md)
10. [Known limits](10-known-limits.md)
11. [Operator runbook](11-operator-runbook.md)
12. [Configuration reference](12-configuration-reference.md)
13. [Image builds and registries (stub)](13-image-builds.md)

Reading order for a first test: 1, 2, 3, then 10 before trusting anything.
