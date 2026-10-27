# E11: restic host backups against real restic and real backends (X14, ADR 0022)

Run: 2026-10-27, `tests/p0/e11-restic.sh`, raw log `e11-restic.log` (sanitized; the script fails the run if a generated secret appears in it). Final run: exit 0, 128 `VERIFIED` lines, no `FAILED` and no `NOT VERIFIED` line. Earlier runs the same day found the bugs listed below; only the last run is the recorded log.

Lab: two VMs `dbcm-lab-m1` / `dbcm-lab-m2` created and removed through `bin/lab` (Debian 12 arm64 Lima `vz`, 2 CPUs, 3 GiB RAM, 15 GiB disk each; limit 7 CPUs, 16 GiB, 100 GiB). Scoped cleanup ran after every run and reported `unrelated resources intact (3)`; `limactl list` afterwards shows only the unrelated `thermoctl-*` instances. No host Docker, host package manager or non-lab VM was touched.

## What ran

- **restic 0.19.1 linux/arm64**, downloaded from the upstream release on both VMs and verified with `sha256sum -c` against the pin that `deploy/Dockerfile` carries (`a5f64aaa...5465`). A deliberately wrong checksum was rejected on both VMs.
- **m1 (agent host):** `cmd/resticlab` drives the real `agent.Executor` (fenced commands, allow-list, secret files, `restic.ExecRunner`, `internal/restic`) as root against real repositories. The Docker side is stubbed; nothing else is faked.
- **m2 (backend host):** sshd (SFTP), rest-server 0.14.0 (arm64, SHA-256 verified, TLS with self-signed certificate, htpasswd bcrypt user), MinIO (TLS). MinIO release binaries return HTTP 410, so MinIO was cross-built from Go module `github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c`; binary SHA-256 `6cfcdda5...df53`. The CA certificate was provided to m1 through `SSL_CERT_FILE`.
- **External host:** m2 itself acted as a host that belongs to no cluster. `TestE11ExternalHostSSHPlanReal` (run on m1 as a compiled test binary) drove the real `HostSourceSSHClient` with a sealed source, a real pinned `known_hosts` and a real SSH session to a non-root user `e11ext`.

## Verified (real)

Per backend (local, SFTP, rest-server, S3/MinIO): `restic init` (operator step); two backups through the agent (incremental second one); snapshots; exclusions (built-in `.ssh` and requested exclude absent from the snapshot); restore without approval refused and nothing created; approved restore into a new path, byte-identical (SHA-256 of every file, symlinks as links) to the source; restore onto the now existing target refused; `forget --keep-last 1 --prune` leaves one snapshot; `check --read-data-subset` 25% and 100%; stats; restore after prune still identical.

Failure and safety cases (local backend unless noted):

- Wrong repository password: backup and snapshots fail, classified as authentication failure. A missing password secret is refused before any restic child starts.
- Wrong backend credentials: rest-server answers HTTP 401, classified `authentication failed`. S3 with a wrong secret: in an earlier run restic retried for more than ten minutes (the HTTP status was not recorded); with the probe (bug 5) the step ends with `repository did not answer in time` after the two-minute bound (final run).
- Interrupted backup: a 1 GiB backup killed 1.5 s after the restic child appeared reports failure and `command interrupted ... a stale repository lock may remain`; no snapshot is created; the repository holds a real lock; `forget --prune` reports `repository locked`; the lock stays; no argv recorded for any restic child start by the runner contains `unlock`. After the operator removed the lock (outside the product, after confirming the owner process was dead) the retried backup completed, `check --read-data-subset 100%` passed and the restored 1 GiB file was byte-identical.
- Allow-list: backup sources `/etc`, `/srv`, a sibling that shares the root as string prefix, `..`, a symlink inside the root pointing to `/etc` (and a path through it), the agent's own secret directory, `/run/secrets`, the Docker socket, `/`, a relative path and a missing path were all refused with `invalid-path`; an exclusion outside the root was refused; no refused request started a restic child. Restore targets outside the roots, with `..`, below the symlink, the root itself, an existing directory and the secrets directory were refused, nothing was created. Hostile snapshot arguments (`--help`, `abc;touch /tmp/pwned`, `../../x`, empty) were rejected and nothing ran.
- Secret hygiene (observed through `/proc` every 20 ms on m1 and `ps` every 50 ms on m2): none of the synthetic repository passwords, rest-server password or S3 secret appears in the argv of any process on either VM; restic children receive `RESTIC_PASSWORD_FILE` (never `RESTIC_PASSWORD`) and only the backend variables of their repository (`AWS_*`, `RESTIC_REST_*`); an unrelated canary variable in the agent's environment was not inherited by any restic child; no secret in any result, message or error the manager would see, nor in the agent state files; password files are `0600`.
- External host over SSH (real OpenSSH, non-root user): backup (exclude honored), second backup, `forget --prune`, `check --read-data-subset`, approved restore to a new path (tree identical to the source except the excluded directory, mode 700), unapproved restore refused by the client, restore onto an existing target failed, restore below `/etc` failed (only by the remote user's permissions, see limits). No lock left behind. The restic password appears in no process list line and no journal line on the remote host (sampled every 50 ms).

## Bugs found by the real run (fixed, with tests that failed first)

1. **SSH runner could not connect to a real OpenSSH server.** The Go client offered its default host key preference (ecdsa first); the server answered with a key that was not the pinned ed25519 one and every SSH-plan operation failed. `provision.SSHRunner` now offers only the key types present in the pinned `known_hosts` (`TestSSHRunnerNegotiatesPinnedHostKeyType`, `TestHostKeyAlgorithmsFollowKnownHosts`).
2. **The 64 KiB SSH output cap did not work and raced.** `limitedBuffer` embedded `bytes.Buffer`, so `io.Copy` used the promoted `ReadFrom` and bypassed `Write` (no cap) and stdout/stderr raced (flaky empty output, found by the race detector). It is now a mutex-guarded buffer without promoted methods (`TestLimitedBufferEnforcesCapThroughIOCopy`).
3. **Remote restic ran without `HOME`.** The generated script used `env -i`, restic died with `neither $XDG_CACHE_HOME nor $HOME are defined`. It now passes `HOME` (`TestSSHResticCommandKeepsHomeForCache`).
4. **Remote restore guard did not guard.** `test ! -e T && test ! -L T && mkdir ...` under `set -e` does not stop at a failing `test` inside an `&&` list, so a restore into an existing directory ran (restic `--overwrite never` skipped existing files but still wrote new ones). It is now an explicit `if ... exit 1` (`TestSSHRestoreRefusesExistingTargetInRealShell`, executes the guard in `/bin/sh`).
5. **Wrong S3 credentials hung for more than ten minutes** (restic retries 403 responses). Each command is now preceded by a bounded `restic cat config` probe (`restic.DefaultProbeTimeout`, 2 minutes; `Config.ProbeTimeout`, `agent.Config.ResticProbeTimeout`) that returns `ErrUnreachable` (`TestProbeBoundsUnreachableRepository`).
6. **A killed command was reported as `command failed`.** A cancelled context is now `ErrInterrupted` with the stale-lock hint (`TestCancelledCommandIsReportedAsInterrupted`).
7. The child environment allow-list now keeps `RESTIC_CACHE_DIR` and `XDG_CACHE_HOME` (the distroless agent image has no home directory).

Not product bugs but harness issues fixed on the way: rest-server 0.14.0 rejected an `openssl passwd -apr1` entry (`Invalid htpasswd entry`), bcrypt via `htpasswd -B` works; a fixed 2.5 s kill timer did not interrupt a fast 400 MiB backup, the harness now kills 1.5 s after the restic child appears and uses 1 GiB.

## Not verified

- The product agent **image** was not built; the Dockerfile restic stage and `COPY` are checked statically by `scripts/check_deploy.py` only. The agent ran as a root process in the VM, not as a Swarm service, so the new opt-in deployment (`AgentMTLS.Restic`: flags, bind mounts, secret mounts, `RESTIC_CACHE_DIR`) is covered by unit tests only, and mounted Swarm secrets were not used.
- amd64: only arm64 binaries ran. The amd64 checksum pin is copied from upstream `SHA256SUMS` and was not executed.
- mTLS enrollment and the fenced HTTP transport of the agent (the executor was called in process); manager scheduler, cron, notifications and approval flow (unit tests with fakes only); concurrency of two sources on one repository.
- SFTP from the **distroless agent image** (it has no `ssh`); SFTP key and host-key provisioning is manual in the lab (ssh-keyscan, authorized_keys).
- Stale-lock reporting, wrong password and wrong credentials over the **SSH plan** transport (only the agent path ran them); a repository on rest-server in append-only mode; S3 with a real cloud provider (MinIO only); S3 and rest-server servers with certificates from a public CA (a private CA via `SSL_CERT_FILE` was used).
- Restic memory use and run time on large data sets (largest file 1 GiB, total about 4 GiB); forget/prune on repositories with many snapshots.
- The SSH path has no remote allow-list: a restore target is limited only by the SSH user's permissions (restore to `/etc/...` failed in the lab only because the user is not root).
- UI (none exists). External-host `agent` transport (503 by design).
