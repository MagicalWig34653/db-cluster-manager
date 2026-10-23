# E4: The real manager and agents create a Galera cluster on a lab Swarm

Date: 2026-10-23. Script: [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh). API lab client: [`tests/p0/e4client`](../e4client/main.go). Logs (sanitized):

- [`e4-manager-galera.log`](e4-manager-galera.log): the **final run from scratch**, every phase passed, exit 0 (02:35Z-02:58Z, code at commit `f228d59`).
- [`e4-manager-galera-clean1.log`](e4-manager-galera-clean1.log): first run from scratch after the development fixes; the backup failed on a worker (F12).
- [`e4-manager-galera-clean3.log`](e4-manager-galera-clean3.log): third run from scratch; the create operation succeeded before the bootstrap task was replaced (F13).
- [`e4-manager-galera-dev.log`](e4-manager-galera-dev.log): the development run (kept VMs, phases re-run after fixes, manual notes for the inspection restore and the resets).

A second clean run (exit 3: the inspection network was missing from the script's manager configuration, a script defect) is not kept.

## What ran

Real components only: `bin/manager` (managed single-instance mode with auth, mTLS agent transport, embedded metastore), the agent as a manager-deployed Swarm global service, MariaDB `mariadb:11.4.13@sha256:250e1609…` and `mariadb-backup` from the job image. The test drove the manager only through its HTTP API, except for operator prerequisites (secrets, networks, host directories, images) and read-only checks.

Environment:

- Host: 14 logical CPUs, 48 GiB, `go1.27.1 darwin/arm64`, `limactl` 2.2.0. Host free disk 67 GiB before, 65 GiB with the VMs and after cleanup (df, 1 GiB resolution).
- VMs: `bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2` (6 vCPUs, 12 GiB, 60 GiB; budget 7/16/100). Ubuntu 26.04 LTS, kernel 7.0.0-28-generic, aarch64, Docker 29.9.2 (rootful, E1 recipe). Swarm: m1 manager (192.168.104.1), m2 and m3 workers.
- Binaries: cross-compiled on the host (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath`), copied with `limactl copy`. **Images built inside m1** (the host Docker daemon was never used): the agent image is the runtime stage of `deploy/Dockerfile` (distroless static nonroot, entrypoint manager) with the host-built `manager` and `agent`; the job image is the final stage of `deploy/Dockerfile.jobs` (tool and uid checks passed, `VOLUME /var/lib/mysql` inherited). Distribution to m2/m3 with `docker save | docker load` through the host (no registry).
- Manager: transient systemd unit in m1, API on `127.0.0.1:18480`, metastore on `127.0.0.1:18490`, enrollment endpoint on `192.168.104.1:7444` (TLS), Docker over the local socket. Passkey RP ID `localhost`, origin `http://localhost:18480`.
- Backup target: SFTP to the OpenSSH server of m1 (dedicated lab user, key login, pinned ECDSA host key). The `dir` backend could not work: a job writes inside its own container, which the manager cannot read. MinIO was not used.

### Host load

The host was loaded by unrelated user processes (Xcode, iCloud `fileproviderd`/`cloudd`, Spotlight); none was touched. Before the development run the 1-minute load average was 94-304 for the full 40-minute wait (polled every 2 min, never below 20), so the development run (22:01Z-23:00Z on 2026-10-22, load 13-124) was started under load. **Timings of the development run are not reliable.** After a pause of about 3 h (coordinator: usage limit) the clean runs started at load 4-15; during the final run unrelated load rose again to 26-62 (5 min: up to 66). Every phase records `uptime` in the log.

## Final run, step by step (all observed)

| Step | Observed |
| --- | --- |
| Setup | `GET /setup/status` true; `POST /setup/begin` + `/setup/finish` with a WebAuthn registration from an ES256 software key: `account admin, 10 recovery codes issued (not printed), session cookie set`; then `setupRequired: false`. (Dev run: after 3 h the session had expired, 401; a passkey login worked.) |
| Swarm | `POST /swarms` (`unix:///var/run/docker.sock`) 201, sync 204, state `healthy`; `GET /hosts`: 3 nodes, roles, 2 CPUs / 4 GiB each, Docker 29.9.2. |
| Enrollment and agents | `POST /hosts/{id}/enroll` for 3 hosts; `POST /swarms/{id}/agents/deploy`: `created: true`, 3 labeled, 0 failed. All three agents enrolled through the real flow (one-time token as Swarm secret, pinned CA fingerprint, CSR, source-address check) within 2 s; `agentIdentity: true` on all hosts. |
| Create | `POST /clusters` (size 3, 1 CPU / 1 GiB limits, gcache 128M, operator network and root secret): 202. Operation `succeeded` after about 80 s (02:38:51Z-02:40:11Z). The bootstrap task was replaced (new task running 23 s before the check) before success. |
| MariaDB | Every node: `Synced`, `wsrep_cluster_size 3`, `Primary`, same state UUID, `@@socket /dbcm/run/mysqld.sock`, `@@datadir /dbcm/data/`. Write on n1 and n3, the same rows read on n1, n2 and n3. |
| Accounts | `dbcm_monitor@localhost` (USAGE only) and `dbcm_backup@%` (`RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR`) on n2, replicated from the bootstrap node's `exec-sql` steps. The monitoring account over the node socket: `CURRENT_USER() dbcm_monitor@localhost`, `wsrep_cluster_size 3`. |
| Volumes | `/var/lib/dbcm/volumes/dbcm-e4db-n1-<uuid8>/`: marker root-owned 0644, `data/` and `run/` uid/gid 999 mode 0750, socket in `run/`. |
| Health in the API | `GET /clusters/{id}`: `healthy`, all nodes `healthy/synced`, endpoints `dbcm-e4db-nN:3306`. |
| Node failure | `docker kill` of the n3 container on m3: t+10 s `degraded`, n3 `unavailable`; t+20-30 s donor n1 `degraded/donor`; t+40 s all `healthy/synced` (Swarm restarted the task, exit 137). **No event or notification** was recorded for the outage. |
| Backup | `POST /clusters/{id}/backups`: 202, `succeeded` in about 15 s. Source n1 on the **worker** m2; the job service was created by the agent on m1 and pinned to m2 (F12 fix). SFTP target: stream 55.7 MB and manifest (tool `mariabackup 11.4.13-MariaDB`, 54 chunks, checkpoints `to_lsn` recorded); warning: `galera_info not found in --extra-lsndir; Galera position not recorded`. `GET /backups`: `lastBackupAt` set. |
| Inspection restore | `POST /restores/inspect` on the manager node: 202 with digest; the operation waited for approval; a wrong digest: `409 approval_mismatch`; the digest: 200. The restore job ran, the instance started with `--wsrep-on=OFF --wsrep-provider=none --port=3307 --skip-slave-start --skip-log-bin --init-file=...` on the separate network; the one-time account `dbcm_inspect@%` read `1 written on n1`, `3 written on n3` with `@@wsrep_on 0`. The operation then waited at the documented verification gate (`restore-verified: not verified yet`, ADR 0011). |
| Cleanup | `bin/lab cleanup -apply` (EXIT trap): removed `dbcm-lab-m1..m3`, `unrelated resources intact (3)`, manifest empty. |

## Failures found and fixed

Each fix is a separate commit with a regression test (`go vet ./...` and `go test ./...` pass on the branch).

| # | Observed | Root cause | Fix (commit) |
| --- | --- | --- | --- |
| F0 | found by code reading before the first run (not observed in a run): every command would have answered `settings_incomplete` | Nothing could set installation ID, volume base, agent image, accounts or backup settings | `manager.settings` configuration merged by the leader (`e4716fc`) |
| F0b | code reading before the first run (not observed) | Operations had `MaxRetries` 0: the first unmet precondition paused; the bootstrap `wait-synced` needed the monitoring account created after it; `exec-sql` not retried; the agent could not mount the root/backup secrets | retries 30; account steps before the bootstrap wait; `exec-sql` idempotent; `agentTransport.dbSecrets` (`e4716fc`) |
| F1 | agent tasks `Rejected`: `bind source path does not exist: /var/lib/dbcm-agent` | Swarm does not create bind sources | Host prerequisite, documented in `docs/INSTALL.md`; the script creates the directories |
| F2 | agents exited: `flag provided but not defined: -manager-url` | The service had only `Args`; the product image's entrypoint is the manager | `Command /usr/local/bin/agent`, `User 0:0` (`41ef159`) |
| F3 | fixed together with F2 before it could show (not observed separately) | distroless nonroot cannot use the Docker socket or chown | same commit |
| F4 | `refused (not-owned): referenced network "dbcm-e4-net" ... not in allow-list` | Nothing put operator objects into the agents' allow-list | resolve referenced networks/secrets (installation label required) and prepend `allow-objects` steps (`898310a`) |
| F5 | joiner: `network "dbcm-e4-net" ... HTTP 404` on a worker | Service commands went to the node's own agent; workers neither see the overlay network nor accept service changes | service commands to an enrolled manager node's agent (`9f0502a`) |
| F6 | backup paused: `backup-source-eligible n1: ... job CPU and memory demand required` | The source re-check lacked the job demand | demand from the operation's run-backup step (`a989c91`) |
| F7 | job: `checkpoints: open .../lsn/xtrabackup_checkpoints: no such file` after a complete upload | MariaDB 11.4 writes `mariadb_backup_*` | both names accepted (`b8e1fea`) |
| F8 | abort accepted (202) but the operation kept waiting for minutes | abort only at step boundaries, not during precondition polls | abort at the next poll (`fd360e2`) |
| F9 | inspection: `not-owned: referenced secret "dbcm-e4-recovery-key"` | as F4, for inspections | allow step before the digest (`2d66fba`) |
| F10 | abort of the failed inspection: 409; the next inspection `resource locked ... held by` it | failed operations have an end time but keep runner locks | abort allowed while the runner holds the operation (`caeb8e6`) |
| F11 | inspection paused at its first `restore-complete` poll | inspection plans had no retry budget | `InspectionRequest.MaxRetries` (`aba52b9`) |
| F12 | clean run 1: backup `failed (docker-error): ... This node is not a swarm manager` | the job service was created by the source node's (worker) agent | run-backup/remove-job on the manager node's agent, pinned with `RunBackupArgs.NodeID`; volume marker still verified by the source host's agent (`96d1353`) |
| F13 | clean run 3: create `succeeded` while the bootstrap node's old task was still running; the next check saw cluster size 2 | `wait-synced` after `update-service` observed the old task | new agent command `await-service` (Swarm `UpdateStatus.State`) between update and the last wait (`f228d59`) |
| - | `TestConversionDropsNothing` failed after F2 (`agent field User ...`) | compat test lists every agent spec field | declared unused for database services (`69b40fb`) |

Script defects fixed on the way (not product code): `/root` paths used as the unprivileged user, a missing wait for leadership, non-idempotent prerequisite creation, the inspection network missing from the manager configuration.

## What remains broken or untested

- **No cluster delete or abandon.** A failed create keeps its cluster record, name and planned capacity; the development run had to wipe the manager state and agents by hand to continue. Failed operations' job services and half-created volumes stay (`dbcm-job-...` 0/1 after an abort).
- **Restore and inspection jobs on worker hosts**: only backup jobs are delegated to the manager node; an inspection or restore whose host is a worker would hit F12. The E4 inspection ran on the manager node.
- Restore verification is not wired (operation waits at the gate); inspection teardown and secret removal were not exercised (VMs were deleted).
- Galera position is not recorded in backup manifests (11.4 does not write `galera_info` to `--extra-lsndir`).
- **Manifests carry SHA-256 digests of plaintext chunks** in clear (21 distinct of 54 chunks in one backup): an observer of the backend can confirm guessed plaintext chunks. Design question for ADR 0009.
- No event, alert or notification for the node outage of 30-40 s. The API's operation view shows only state and the last reason; while a step polls, `reason` is stale (debugging needed log access).
- Not exercised: node replacement, outage diagnosis, backup schedule, logical backup, restore into a new cluster, certificate renewal/revocation, agent service update for new secrets, more than one manager replica, the UI, amd64, `REQUIRE SSL`.
- Lima forwards guest listeners on loopback and `0.0.0.0` to host loopback (E3): the manager API (authenticated), the **unauthenticated metastore port** `127.0.0.1:18490` and the agents' mTLS port were reachable from the host's loopback during the runs.

Follow-up (after this run, code and fakes only, not re-run here): cluster delete/discard, restore and inspection jobs on the manager node, Galera position from the stream, node-outage events and notifications, operation progress, restore verification and init secret teardown; see `docs/STATUS.md`, "E4 gaps closed". The plaintext chunk digests and the Lima loopback exposure remain.

## Cleanup and unrelated resources

Every VM was created and removed with `bin/lab` only (development VMs: `bin/lab cleanup -apply` before the clean runs; each clean run: EXIT trap). Final `bin/lab status`: manifest empty; unrelated (never touched): `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm` (both thermoctl VMs `Stopped` before and after). The shared Lima user-v2 network state is not a lab resource and was left alone. Host-side build artifacts lived in the git-ignored `.lab/e4/` and were deleted after the run.
