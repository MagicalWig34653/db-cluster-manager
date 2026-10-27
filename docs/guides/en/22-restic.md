---
title: Restic host backups
---

# Restic host backups [Fakes]

The manager has a native restic source model for filesystem backups of an individual managed host. A source belongs to a host, not a MariaDB cluster. It records source paths, a local/SFTP/S3/rest-server repository, UTC cron schedule, retention and an optional `check --read-data-subset` percentage. The manager seals password and backend credentials in replicated metadata and never returns them through the API. Its runner sends fenced steps to the enrolled host agent; errors trigger a notification. [ADR 0022](../../decisions/0022-native-restic-host-backups.md) records the decision and limits.

The agent execution environment (normally its container) needs a restic binary, `-restic-allow-path /absolute/root` for each allowed source/restore root, and named local secret files in its configured `-db-secrets-dir`. The repository password file name is `passwordSecret`; optional S3 or rest-server credentials come from a JSON secret file named `credentialSecret`. The current product agent image does not package restic, and manager deployment does not add these mounts, allow-list flags or secrets to an existing global agent service. Creating a source through the API therefore does **not** make an existing deployment executable. Repository initialization and SFTP SSH key/known-host provisioning are also not automated.

## API and permission

`GET /api/v1/host-sources` and `GET /api/v1/host-sources/{id}` return metadata visible to the session. An administrator can `POST /api/v1/host-sources`, `PUT /api/v1/host-sources/{id}` and `DELETE /api/v1/host-sources/{id}`. `password` and `credentials` are write-only fields on create/update; their secret names are separate from their values. A scoped operator can `POST /api/v1/host-sources/{id}/backups` to start a runner operation. A viewer can only read. Every state-changing permission decision is audited.

Example source body (synthetic credentials only):

```json
{
  "id": "host-home",
  "name": "Home directory",
  "kind": "managed-host",
  "transport": "agent",
  "swarm": "sw1",
  "hostId": "node-1",
  "paths": ["/srv/home"],
  "excludes": ["/srv/home/cache"],
  "backend": "s3",
  "repository": "s3:backup.example.test/host-home",
  "passwordSecret": "restic-home-password",
  "credentialSecret": "restic-home-s3",
  "password": "synthetic-example-only",
  "credentials": {"AWS_ACCESS_KEY_ID": "synthetic", "AWS_SECRET_ACCESS_KEY": "synthetic"},
  "schedule": "0 2 * * *",
  "retention": {"keepDaily": 7, "prune": true},
  "checkSubset": "5%"
}
```

`POST /api/v1/host-sources/{id}/restore` takes `{"snapshot":"<restic-id>","target":"/srv/new-path"}`. It creates an operation whose step waits for a separate digest approval through the existing operation approval endpoint. The agent refuses an existing target and paths outside its allow-list. It atomically creates the new target and uses `--overwrite never`; never point it at live data. A successful subset check is repository-integrity evidence, not a test restore.

`external-host` sources have no Swarm field. With `transport: "ssh-plan"`, provide `sshAddress`, a non-root `sshUser`, and write-only `sshPrivateKey` and pinned `sshKnownHosts` on creation. The manager seals those values and sends the restic password and backend credentials over SSH stdin. The remote script creates a private temporary password file, invokes restic and removes that file; it never embeds secret values in the SSH command. This SSH path has fake tests only and is subject to a 64 KiB output cap. `transport: "agent"` remains unavailable (503) until independent external-host enrollment exists. No UI is present. No command here has been run against real restic, an S3/SFTP/rest-server target or an external host. Keep using the existing [database backup guide](05-backup-restore.md) for cluster data.
