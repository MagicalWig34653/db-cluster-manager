---
title: Restic host backups
---

# Restic host backups [Lab-verified on arm64 Debian 12 and through the manager with the native agent; deployment wiring partly open]

The manager has a native restic source model for filesystem backups of an individual managed host. A source belongs to a host, not a MariaDB cluster. It records source paths, a local/SFTP/S3/rest-server repository, UTC cron schedule, retention and an optional `check --read-data-subset` percentage. The manager seals password and backend credentials in replicated metadata and never returns them through the API. Its runner sends fenced steps to the enrolled host agent; errors trigger a notification. [ADR 0022](../../decisions/0022-native-restic-host-backups.md) records the decision and limits.

## Making a deployment executable (opt-in)

The agent execution environment (normally its container) needs a restic binary, `-restic-allow-path /absolute/root` for each allowed source/restore root, and named local secret files in its configured `-db-secrets-dir`. The repository password file name is `passwordSecret`; optional S3 or rest-server credentials come from a JSON secret file named `credentialSecret`.

- **Binary.** The product image (`deploy/Dockerfile`) now contains a pinned restic (`RESTIC_VERSION`, `RESTIC_SHA256_AMD64/ARM64` build arguments) downloaded from the upstream release and verified with `sha256sum -c`; the build stops on a mismatch. `scripts/check_deploy.py` checks the pin. The image has no `ssh` client, so **SFTP repositories need a different agent image** (or one of the other backends). The distroless image has no shell; restic is a static binary and runs without one.
- **Allow-list flags, mounts and secrets.** Restic is off unless the agent deployment opts in. `swarm.AgentMTLS.Restic` (`AgentRestic{AllowPaths, Secrets}`) adds one `-restic-allow-path` flag **and one bind mount at the same path** per root, mounts the named existing Swarm secrets below `/run/secrets`, and sets `RESTIC_CACHE_DIR` below the agent state directory. Roots must be clean absolute paths below `/`; the Docker socket, `/run/secrets` and the agent state directory are refused, as is an empty list. Without the option the service spec is unchanged. Mounts are read-write because new-path restores write there.
- **Not automated.** The manager does not create the Swarm secrets from the sealed source credentials, does not update an already created global agent service (the deployment call only creates it; changing roots needs a service update or a re-create), and does not initialize repositories or provision SFTP keys. The operator runs `restic init` once per repository. The wiring of `AgentRestic` into a manager configuration file or UI is not implemented (library level only; unit-tested).
- **Bounded start.** Each restic command is preceded by a bounded `restic cat config` probe (2 minutes). Real runs showed that wrong S3 credentials made restic retry for more than ten minutes; the step now fails with a clear "did not answer in time" message instead of hanging. A cancelled or killed command is reported as interrupted and warns about a possible stale lock; the wrapper never runs `unlock`.

[Lab run E17](../../../tests/p0/results/e17-restic.md) went through the product path (module switch, host-source API, native agent as root with the unit hardening, S3 on MinIO over TLS): backup, retention, check, approved restore into a new path and verification passed after two fixes; S3 locations must use https (`s3:http://` is refused). [Lab run E11](../../../tests/p0/results/e11-restic.md) exercised the real restic wrappers against local, SFTP, rest-server (TLS) and S3 (MinIO, TLS) repositories, and an external host over SSH. See "Verified" there for exactly what was and was not covered.

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

`external-host` sources have no Swarm field. With `transport: "ssh-plan"`, provide `sshAddress`, a non-root `sshUser`, and write-only `sshPrivateKey` and pinned `sshKnownHosts` on creation. The manager seals those values and sends the restic password and backend credentials over SSH stdin. The remote script creates a private temporary password file, invokes restic and removes that file; it never embeds secret values in the SSH command. The SSH path is subject to a 64 KiB output cap and has no allow-list on the remote side: the remote account's own file permissions are the only restriction for sources and restore targets (use a dedicated non-root user). It needs the pinned `known_hosts` entry to contain the host key type the server actually presents; the runner now offers only the pinned key types. It was run once against a real OpenSSH server (lab E11). `transport: "agent"` remains unavailable (503) until independent external-host enrollment exists. No UI is present. Keep using the existing [database backup guide](05-backup-restore.md) for cluster data.

## Agent runtime (ADR 0024)

Host sources show `runtime` (`native` or `container`) and `runtimeNote`: restic runs on the native agent when it reports the `restic` capability, otherwise on the container agent, never silently elsewhere. If the host reports neither, starting a backup answers 409 `agent_runtime_unavailable`. See [guide 24](24-agent-runtimes.md).
