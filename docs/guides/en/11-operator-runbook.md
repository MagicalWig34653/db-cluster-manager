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

## 4. Remove a manager installation

`docker compose -f deploy/compose.yaml down` (add `-v` to delete the `manager-data` volume), or `docker stack rm dbcm` and the volume on each node. The manager does not remove agents, database services or volumes it created. Delete clusters through the API first (approval needed, see [Creating a cluster](03-create-cluster.md)); remove the agent service, host directories under the volume base, and the operator networks and secrets by name. Never prune globally.
