---
title: Disaster recovery and backup copies
---

# Disaster recovery and backup copies

[Guide index](index.md) | Previous: [Provisioning](19-provisioning.md)

Most of this guide is verified with fakes only [Fakes]: in-memory stores, an `httptest` S3 server, an in-process SFTP server and an in-memory Raft replica. Lab E14 ([results](../../../tests/p0/results/e14-backup-restore.md)) additionally ran, on three real VMs with MinIO over TLS: backup copy to a `local` target with deep digest verification (a flipped byte was caught by the restore proposal), the restore proposal, scheduled restore verification and retention [Lab VMs]. Not run on real systems: executing a restore of a copy, metastore backup and import, SFTP, PITR. Design: [ADR 0021](../../decisions/0021-backup-targets-copies-and-metastore-backup.md).

## What you need to keep outside the manager

| Item | Why |
| --- | --- |
| The **backup recovery key** (private half; the manager only has the public half in `settings.backup.recipientKey`) | Opens cluster backups, metastore backups and the data-key recovery envelopes. Without it all of them are unreadable. |
| The **backend configuration files** (S3/SFTP credentials, optional `encryption` key) | Reach the targets and decrypt client-side encrypted objects. |
| The manager configuration file (`manager.json`) and the list of Swarm secrets and networks you created | Names, not values. |

## Backup targets

`manager.backup.backendConfigFile` is the main target. `manager.backup.targets` adds named targets (name to backend config file) for copies and for metastore backups. A backend config has a type `s3`, `sftp`, `local` (alias `dir`) and an optional client-side encryption:

```json
{"type": "sftp", "sftp": {"addr": "backup.example:22", "user": "dbcm", "dir": "/srv/dbcm",
  "host_key_fingerprint": "SHA256:..."},
 "encryption": {"key_id": "t1", "key": "<base64 of 32 random bytes>", "previous": {"t0": "<base64>"}}}
```

With `encryption`, every object (manifests, status files, transfer and metastore records) is encrypted before it leaves the process; the target sees only ciphertext and names. After a key change keep the old key under `previous` so older objects stay readable. Cluster data streams are always encrypted for the recovery key. A `local` target does not work for backup jobs, which write from inside their own container (see [Backups and restore](05-backup-restore.md)).

## Copy a backup to another cluster, Swarm or site

1. List your targets in `manager.backup.targets` and restart the manager.
2. `POST /api/v1/clusters/{id}/backup-copies` with `{"backup": "<id>", "target": "<name>"}` (permission: backup on the cluster, audited). It answers 202; poll `GET /api/v1/backup-copies`.
3. The leader copies the stream, checks it against the manifest digest while copying (a mismatch deletes the copy and fails the run), then writes the manifest and, last, an HMAC-signed transfer record. A failed or interrupted copy records an event and a notification (`backup.copy_failed`) and can simply be started again.
4. Retention of the source set deletes its copies from the targets (transfer record first). The newest and the newest verified set keep theirs.

A copy changes nothing: no cluster, no route, no proxy.

## Restore from a copy: explicit approval, never automatic

1. `POST /api/v1/backup-copies/{id}/restore-proposal` with `{"targetSwarm": "...", "targetCluster": "new-name"}` (permission: restore, audited). The manager verifies signature, manifest and stream digest and returns a **proposal** with a digest. `approvalRequired` is always `true`, `autoPromotion` always `false`. The name must not belong to an existing cluster.
2. A person reviews it. Approval binds to the exact digest; the restore plan always creates a **new** cluster with a new identity, refuses the source cluster's identity, names, networks and volumes, and every destructive step waits for the approval ([Backups and restore](05-backup-restore.md)).
3. Switching clients to the restored cluster is a separate, manual decision. The restored cluster starts a new Galera history.

### Test a copy in an isolated instance

To prove that a copy can be read, restore it into an isolated inspection instance (the same operation as an [inspection restore](05-backup-restore.md); nothing is promoted and the cluster is not touched):

1. Give every target its own Swarm secret with the backend configuration of that target and name it in `settings.backup.targetSecrets` (`{"dr": "dbcm-backend-dr"}`; each name must exist in `manager.backup.targets`). Without an entry the restore is refused with `settings_incomplete`; the manager never uses the primary secret for a copy.
2. `POST /api/v1/restores/inspect` with `{"cluster": "...", "host": "...", "copy": "<copy id>"}` (permission: restore on the source cluster, audited). In the UI choose "Copy on a second target" in the restore wizard. `backup` and `pointInTime` must stay empty: the binary log archive is not copied, so a point in time is refused for copies.
3. Before planning, the manager verifies the copy completely (signed record, manifest, digest of the stored object); a copy that does not verify answers `409`. The approval digest covers the secret and the object. The restore job reads the copy through the target secret, and the operation reads its result from that target.

[Fakes]: the proposal, approval and plan exist in code and tests; the HTTP endpoint returns the proposal only. Executing the restore of a copy into a **new cluster** through the API is not wired yet. The inspection restore of a copy is tested with fakes and in-memory backends only (no VM, no real second target). The copy routes (`/backup-copies`, `/clusters/{id}/backup-copies`, `/backup-copies/{id}/restore-proposal`) and the metastore backup routes are mounted in the managed runtime whenever `manager.backup` is configured (a test starts the real runtime with a `targets` entry and reaches them; creating a metastore backup still answers 409 `settings_incomplete` until `settings.backup.recipientKey` is set).

## Scheduled restore verification

Add `verify` to the backup policy of a cluster (`PUT /api/v1/clusters/{id}/backup-policy`):

```json
{"enabled": true, "schedule": "0 2 * * *", "verify": {"enabled": true, "schedule": "0 4 * * 0", "maxMinutes": 90}}
```

On that schedule the leader restores the newest completed physical backup into an isolated inspection instance (same plan as a manual inspection: wsrep off, own network, production accounts locked), lets the verification gate probe it and tears it down again. Enabling it needs the restore permission and is recorded as the approval of these isolated inspections. A failed check, a failed or aborted operation, a run that exceeds `maxMinutes` or one that cannot start records an event and sends a notification (`backup.restore_verification_failed`); a pass sends `backup.restore_verification_passed` (info). Limits: the report is instance-level; table contents are not compared, and an inspection host must be given (`verify.host`, the Swarm node ID or, since 2026-10-31, the host name, both shown by `GET /api/v1/hosts`; an unknown value fails with "not enrolled in the cluster's Swarm") because free disk space is not observed.

## Back up the manager's own metastore

`POST /api/v1/settings/metastore-backups` (permission: backup on settings) or, with `manager.backup.metastoreBackupHours`, on a schedule. It needs `settings.backup.recipientKey` and a target (`metastoreTarget`, default the main backend). The export contains clusters, hosts, Swarms, backup policies and records, settings, accounts, roles, passkeys, the agent CA, the keyring and sealed secrets. It leaves out sessions, operations, approvals and events, so nothing resumes by itself after a restore. It is encrypted for the recovery key, so the running manager cannot read it again. `GET /api/v1/settings/metastore-backups` lists backups whose signed record verifies; `metastoreBackupKeep` (default 14) bounds how many stay. A failing scheduled backup records an event and a notification (`backup.metastore_failed`).

## Runbook: the manager is lost

1. **Decide that you need it.** If one replica of several is lost, do not restore: use membership to replace it. Restore only when no replica with the data remains. [Untested]
2. **Prepare a host** that can run the manager (a Swarm manager node with the Docker socket, same image). Put the configuration file in place. Keep the old data directory aside if any exists; the import refuses a non-empty `metastore` directory.
3. **Fetch** the newest `metastore-<installation>-<time>.dbcms` from the target and its `.record.json`. Put the recovery key into a file readable only by you.
4. **Import** with the manager stopped, as the user that runs it:

   ```
   manager metastore-import -config /etc/dbcm/manager.json \
     -in metastore-inst-20261026t020000.dbcms -recovery-key-file /safe/recovery.key
   ```

   It opens the export, starts a single-voter metastore on `manager.bind`, writes the documents (keyring first, data keys re-wrapped for this replica's identity `replica.key`, created if missing), prints what it did and stops. Delete the key file afterwards.
5. **Start the manager.** Log in with a passkey or recovery code (sessions were not restored). Re-register nothing else: the single replica is registered by the import. Add further replicas through the normal membership procedure.
6. **Reconcile before you act.** The restored state is as old as the backup. Compare hosts, services and clusters with the live Swarm (`Sync Swarm`, drift reports) and check that the agents' certificates still work. Operations that were running at export time are gone; look at each cluster's health before starting anything. Re-enroll hosts whose certificates changed after the backup.
7. **Check backups.** Run a backup and an inspection restore for one cluster, and a metastore backup, to prove the chain again.

Not covered: a lost recovery key (then neither cluster backups nor metastore backups can be opened), a lost keyring without the recovery envelope (secrets stay unreadable), a restore into a manager of a different installation ID, and a lost Swarm (rebuild the Swarm first; the agents and secrets are not restored by this procedure).

## Verification levels

| Part | Level |
| --- | --- |
| Signed documents, client-side encrypted backend over local, S3 (`httptest`) and SFTP (in-process) targets, tamper/truncation/swap tests | [Fakes] |
| Copy between backends, verification, retention coupling, restore proposal with approval gate, API, OpenAPI and authorization tests | [Fakes]; copy, deep verification, proposal and retention also [Lab VMs] (E14) |
| Scheduled restore verification with failure notification | [Fakes]; a scheduled verification passed on real VMs, and a wrong host produced the failure event [Lab VMs] (E14) |
| Metastore export/import, offline `metastore-import`, scheduled metastore backup | [Fakes] (one in-memory Raft replica); the runbook has never been run on a real manager |
| Inspection restore of a copy (`copy`, `targetSecrets`, deep verification, backend routing of the gates, PITR refused, UI choice) | [Fakes] only; no VM run |
| Restore from a copy into a new cluster executed through the API; cross-installation verification | [Untested] |
