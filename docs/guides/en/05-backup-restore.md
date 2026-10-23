---
title: Backups and restore
---

# Backups and restore

[Guide index](index.md) | Previous: [Health and alerts](04-health-alerts.md) | Next: [Node replacement](06-node-replacement.md)

## Concepts

- **Cluster backup:** a physical backup (`mariabackup`) streamed, encrypted, to an S3 or SFTP backend by a one-shot Swarm job service (job image `deploy/Dockerfile.jobs`, `mariadb:11.4.13` plus `dbcm-job`). A **logical** backup (one `mariadb-dump` export per named database) is available as a method; it needs `accounts.backupLogical` in the settings so the backup account has the read privileges.
- **Manager configuration backup** is a separate library feature (see below).
- **Inspection restore:** the backup is restored into an isolated single-node instance (`--wsrep-on=OFF`, own network, own port, no replication, no binary log) so you can look at data. It never joins the production cluster.

## Prerequisites

Backend settings (`manager.backup.backendConfigFile`), a recovery-key Swarm secret, a backup option file secret, the job image on the relevant nodes, and an inspection network. The lab used SFTP with a dedicated user and a pinned host key. The `dir` backend does not work with jobs (the job writes inside its own container).

## Start a backup and set a schedule

- Now: `POST /api/v1/clusters/{id}/backups` (permission: backup on the cluster). The manager picks an eligible source node and creates an owned job pinned to that host.
- Schedule: `PUT /api/v1/clusters/{id}/backup-policy` with `enabled`, a five-field cron-like `schedule`, `timezone`, `missed` (`skip` or `run-once`), `method: physical` or `logical` (logical needs `databases`, at most 64), and an optional `retention` with `keepLast` (1-1000), `keepDaily` and `keepWeekly`. A manual logical backup is `POST /api/v1/clusters/{id}/backups` with `{"method": "logical", "databases": ["app"]}`. `GET /api/v1/backups` lists `lastBackupAt`, `nextRunAt`, `lastVerifiedRestoreAt`; absent means unknown, not "none".

![Backups page](../../assets/screenshots/en-light-backups.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

## Retention

After each scheduler pass the manager deletes older complete backup sets according to the policy (manifest first, then stream). It always keeps the newest set, the newest set with a passed restore verification and sets used by an open inspection, and skips a cluster while one of its operations is unfinished. [Fakes]: an end-to-end test over four simulated days with fake jobs and an in-memory backend; no deletion ran against a real SFTP or S3 backend.

## Restore with inspection and approval

1. `POST /api/v1/restores/inspect` with `cluster`, optional `backup` (default: newest completed), `host`, `keySecret`, `ttlMinutes` (at most 1440). The response contains the **approval digest** and, only once, the inspection account password.
2. Review the plan, then `POST /api/v1/operations/{id}/approve` with the digest. A wrong digest answers `409 approval_mismatch`. Approval needs the **restore** permission (administrators; not in the operator preset).
3. The job restores, the instance starts, and you connect with the one-time `dbcm_inspect` account. At expiry the instance, its volume and its init secret are removed.

![Operations with approval and progress](../../assets/screenshots/en-light-operations.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

## Verification levels

| Part | Level |
| --- | --- |
| Physical backup to SFTP on a worker-hosted source (about 15 s, 55.7 MB stream, manifest with checkpoints) | [Lab VMs] (E4) |
| Inspection restore with approval digest, wrong digest `409`, restored rows readable, `wsrep_on 0` | [Lab VMs] (E4, on the manager node's host) |
| Restore and inspection jobs on worker hosts (job on the manager node's agent, check of the target host) | [Fakes] |
| Restore verification gate, instance-level report, `lastVerifiedRestoreAt`, teardown with secret removal | [Fakes]; table contents are not compared even in the design |
| Galera position in the manifest read from the stream | [Fakes]; unconfirmed assumption for MariaDB 11.4 |
| Backup scheduler, logical backup (manual and scheduled), retention, S3 backend, restore into a **new cluster** | [Fakes] |
| Selective restore, PITR, key rotation and re-encryption | [Untested] (not implemented) |
| Full restore of production-size data, restore timing | [Untested] |

A backup is not proven good until a restore has been tested. Until a verified restore exists for your data, treat the backup as unverified. Manifest format 2 ([ADR 0009 amendment](../../decisions/0009-cluster-backup-method.md)) stores chunk and plaintext digests as HMACs keyed from the per-stream data key, so the manifest no longer reveals plaintext hashes [Fakes]; format 1 manifests stay restorable and verified. The lab backups of E4 predate format 2 and are format 1 [Untested on VMs].

## Manager configuration backup

`internal/backup` can create encrypted backups of the manager configuration to local, S3 or SFTP backends and restore them in inspection mode. Nothing schedules it and no UI or API exposes it [Fakes]. See the [operator runbook](11-operator-runbook.md) for what to do instead.
