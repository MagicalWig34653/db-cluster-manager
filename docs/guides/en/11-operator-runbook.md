---
title: Operator runbook
---

# Operator runbook

[Guide index](index.md) | Previous: [Known limits](10-known-limits.md)

Procedures for whoever runs the manager. Each step states what is verified. Where no product feature exists, the manual fallback is labelled [Untested]: it follows from how the state is stored, but nobody has run a restore from it.

## 1. Back up the manager state

**Product feature:** `internal/backup` can write encrypted manager-config backups, but no scheduler, API or UI calls it [Fakes]. The "Fresh install and configuration restore documented and reproduced" R1 gate is therefore open.

**Manual fallback [Untested]** (managed mode, single instance):

1. Note the paths in your configuration: `manager.dataDir` (metastore, `data.key`, monitoring store) and the configuration file.
2. Stop the manager for a consistent copy (the state is a Raft log plus snapshots in bbolt; do not copy a running instance).
3. Copy `dataDir` and the configuration file to storage outside the host. **`data.key` decrypts accounts, sessions, passkeys and notification secrets: store the copy encrypted and separate from the database backups.**
4. Start the manager and check `GET /api/v1/health`.
5. Also keep: the recovery key secret used for cluster backups, the backend configuration file (S3/SFTP credentials), your recovery codes, and the list of Swarm secrets and networks you created (their names, not the values).

Test the restore on a scratch machine before you need it: copy `dataDir` back with the same `nodeId`, start the manager, log in with a passkey.

## 2. Upgrade the manager

No upgrade has been run since state exists, and no schema migration policy is documented [Untested].

1. Read the release notes and [Known limits](10-known-limits.md). Do the backup in section 1 first.
2. Check the Docker compatibility matrix against your Docker version.
3. Single instance, container: `DBCM_IMAGE=<new> docker compose -f deploy/compose.yaml up -d` (volume kept). Process install: replace the binary and restart the unit. Verify `GET /api/v1/version` and `/api/v1/health`.
4. The agents run the image named in the manager settings. After changing the agent image, run `POST /api/v1/swarms/{id}/agents/deploy` and confirm that every host reports `agentIdentity: true` [Untested].
5. Running operations survive a restart only as far as the runner resumes them (`autoResume`); check `GET /api/v1/operations` and resume or abort pending ones. Do not upgrade during a backup, restore or recovery.
6. Rollback: restore the previous image and the state copy from step 1. Major-version upgrades will need explicit approval once they exist.

Database (MariaDB) upgrades are not implemented; major upgrades always need your explicit approval.

## 3. Lab cleanup (test environments only)

Lab VMs are created and removed only with `bin/lab`, which keeps an ownership manifest (`.lab/manifest.json`) and only touches names starting with `dbcm-lab-`.

```sh
make build
bin/lab status             # read-only: host capacity, budget, manifest, unrelated Lima/Colima entries
bin/lab cleanup            # preview of the manifest-scoped teardown
bin/lab cleanup -apply     # executes it; fails if an unrelated entry disappeared
```

The experiment scripts under `tests/p0/` create and remove their own VMs and run `bin/lab cleanup -apply` in an exit trap [Lab VMs]. Rules: respect the lab budget (16 GB RAM, 50% CPU, about 100 GB disk), never use `docker system prune`, never remove unrelated Colima or Lima profiles, and keep production credentials and backups out of the lab. Reclaimed disk space after cleanup was not verified (the free space was lower afterwards in W04). Host-side build artifacts in the git-ignored `.lab/` directory can be deleted by hand.

## 4. Host maintenance and drift

**Host maintenance** [Fakes]: `GET /api/v1/hosts/{id}/maintenance-plan` previews, per cluster with a node on the host, the steps, warnings and why a cluster refuses (fewer than a strict majority of its nodes outside the host, an unfinished operation). `POST /api/v1/hosts/{id}/maintenance` (optional `reason`) stops those nodes gracefully one at a time, each only after every node of the cluster on other hosts is Synced; replacement on that host is paused. It refuses with 409 and changes nothing if any cluster refuses. `Host.maintenance.ready` says when you may reboot the host yourself: **the product never reboots or drains a host and does not change its Swarm availability.** `DELETE /api/v1/hosts/{id}/maintenance` checks each volume identity marker, starts the nodes one at a time and waits for Synced (IST or SST). Needs operate permission on the host and its clusters; audited.

**Drift** [Fakes]: `GET /api/v1/clusters/{id}/drift` compares the cluster record with the Swarm services carrying its label (missing, recreated outside the manager, foreign labels, changed placement, scaled to 0 or not running outside maintenance and operations, unexpected services). Each finding proposes a manual action; nothing is changed. The leader also checks periodically and records an event when the findings change.

## 5. Replicas, key ring and peer certificates

None of this has run with several manager replicas on VMs [Untested]; the pieces are tested with fakes and loopback TLS [Fakes]. Reference: [configuration reference](12-configuration-reference.md), [ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md).

1. **Peer certificates:** `DBCM_PEER_CA_PASSPHRASE=<at least 12 characters> manager -issue-peer-certs ./peers -installation inst-1 -peer-ids manager-1,manager-2,manager-3`. This writes `ca.pem`, `ca.sealed` (the encrypted CA) and one directory per replica with `peer.pem` and `peer-key.pem`. Copy each replica directory to its host; keep `ca.sealed` and the passphrase offline. The command refuses to overwrite an existing CA.
2. **Configuration:** on every replica set `peers` (the same list), `bootstrap: true` for the first start, `keyring: true` and `peerTLS`. Peer certificates are not renewed by the product: reissue them with the sealed CA before they expire (default validity 365 days); the replica re-reads a changed certificate file without a restart.
3. **First replica:** start it. An existing `data.key` is adopted as the first key of the keyring; no new key file is created.
4. **Register further replicas:** on each, run `manager -print-replica-key -config <file>` (creates `<dataDir>/replica.key` if needed) and note the public key and its fingerprint. Compare them out of band, then as an administrator call `PUT /api/v1/settings/replicas/{id}/data-key-recipient` with the public key (`dbcmpk1:...`). Only current voters are accepted; nothing is registered automatically. A replica that becomes leader without the key hands leadership over, so an unregistered replica will not serve the API. `GET /api/v1/settings/replicas` shows the keyring status and whether the keys are `recoverable`.
5. **Recovery:** with `settings.backup.recipientKey` configured, every data key also has an envelope for the backup recovery key; keep that private key outside the cluster. Back up `<dataDir>` as in section 1.
6. **Cut off a replica:** `POST /api/v1/settings/replicas/{id}/revoke-certificates` denies its manager certificates at every agent. It does not take back keys the replica already unwrapped; rotation is not implemented.

## 6. Remove a manager installation

`docker compose -f deploy/compose.yaml down` (add `-v` to delete the `manager-data` volume), or `docker stack rm dbcm` and the volume on each node. The manager does not remove agents, database services or volumes it created. Delete clusters through the API first (approval needed, see [Creating a cluster](03-create-cluster.md)); remove the agent service, host directories under the volume base, and the operator networks and secrets by name. Never prune globally.
