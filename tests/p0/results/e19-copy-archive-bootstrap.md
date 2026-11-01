# E19: restore of a backup copy, archive schedule with multi-file PITR and gap, fresh-server bootstrap through the API, 2026-11-01

Lab VMs: `bin/lab` (arm64, 2 vCPU / 4 GiB / 20 GiB, Docker 29.9.2, MariaDB 11.4.13). Parts 1 and 2: three VMs (E4 layout: real manager as a process in m1, mTLS agents, Swarm). Part 3: two VMs (`-templates debian-12,docker`, a fresh Debian 12 server and the manager host). Both sets were removed with `bin/lab cleanup --apply`; the three unrelated Lima entries were intact each time. Scripts: [e19-copy-archive.sh](../e19-copy-archive.sh) (infrastructure), [e19-bootstrap.sh](../e19-bootstrap.sh), driver `tests/p0/e19/drive.mjs` (Playwright/Chromium with a virtual WebAuthn authenticator and the browser session for API calls; Playwright is not a repository dependency), `tests/p0/e19/s3tool.py` (SigV4 client to tamper with and delete stored objects). Raw logs: [e19-infra.log](e19-infra.log), [e19-drive.log](e19-drive.log), [e19-bootstrap-setup.log](e19-bootstrap-setup.log), [e19-bootstrap-drive.log](e19-bootstrap-drive.log), screenshots in [e19/](e19/).

**Host conditions (honest).** Foreign host load was 25-150 (1 minute) during the whole day, far above the rule "below 40". Part 1/2 started after the load dropped to 34 and ran at 21-25; part 3 started at a load of 82 because I did not check again (a mistake; two small VMs, no measurable effect noted). Free disk 118-122 GiB. The lab budget was kept (12 GiB RAM, then 8 GiB).

## Part 1: backup copy to a second target and restore of the copy

Setup: two MinIO servers with TLS from a **private CA** created in m1 (main target on m2, copy target `copy2` on m3, different credentials and prefixes `e19-`/`e19c-`, the CA only in the backend files as `ca_pem`; a request without the CA failed). Cluster `e19db` with **binlog=true** (`log_bin 1`, `ROW`, size 3, created through the API in 70 s). Data `e19.orders` 30,000 and `e19.items` 500 rows.

| Step | Result |
| --- | --- |
| Backup now, copy to `copy2` | both succeeded in 3-5 s; the copy has stream, manifest and signed transfer record under `e19c-` on m3 only |
| Manager **without** `settings.backup.targetSecrets`: `POST /restores/inspect` with `copy` | **409 `settings_incomplete`**, no operation left behind. The main backend secret was never used as a replacement. (The OpenAPI text said 503; the status is 409 like everywhere else, the text was corrected.) |
| Manager restarted **with** `targetSecrets {copy2: dbcm-e19-backend-copy}`; restore of the copy through the **UI** (wizard, "Copy on a second target", select shows the copy, host m2, acknowledgement, "Run inspection", approval card with the digest) | `POST /restores/inspect` body had only `cluster`, `host`, `copy`; operation `dbbackup/inspection` 11 steps in 12 s; the restore job service mounted exactly `dbcm-e19-backend-copy` and the recovery key, **not** `dbcm-e19-backend` |
| Comparison on the restored instance (`wsrep_on 0`) | rows 30000 / sum(amount) 14899275 / sum(id) 450015000, items 500, `CHECKSUM TABLE` orders 102215110 and items 3973433316: **identical to the source** |
| One byte of the stored copy flipped (first through the MinIO data file, then, because that changed nothing visible, properly through the S3 API with `s3tool.py`) | `POST /restores/inspect` with the copy: **409 `conflict`**, no operation created; `restore-proposal` of the copy: 400 `bad_request` (the endpoint refuses a request body that is empty; not investigated further) |

Observation: the first tamper attempt edited a chunk file inside MinIO's own data directory; the manager still reported the copy as valid because MinIO serves the object from its own metadata. Only the object written through the S3 API counts as tampering. Recorded so nobody repeats it.

Not done in part 1: restore of a copy into a **new cluster** (still proposal only), a copy to SFTP or the `local` target (E14 did `local`), a copy of a chain with binary logs (the archive is not copied by design), the tampered copy with a wrong secret instead of a flipped byte (tested with fakes).

## Part 2: archive schedule in the backup policy, PITR across several files, gap

`PUT /clusters/{id}/backup-policy` with `archive {enabled, schedule "*/2 * * * *"}` (the full backup schedule `0 4 * * *`); the leader's scheduler started a `dbbackup/binlog-archive` operation every 2 minutes (09:16:29, 09:18:29 ... all succeeded in about 3 s each). Between runs I wrote five batches (insert 800, update, delete) and recorded the state with `wsrep_sync_wait` at five timestamps; five archive files (`...-l000001 ... l000005`, each with its manifest) landed on the main MinIO.

| Step | Result |
| --- | --- |
| `pitr-proposal` for each of the 5 times | `covered true`, 1 to 5 segments, coverage 07:12:41Z to 07:24:30Z |
| **Middle point** (batch 3, 07:18:41Z) restored through `POST /restores/inspect {pointInTime}` with approval | operation succeeded in 18 s; rows 31470, sum(amount) 14546634, sum(id) 917036895, batches B1,B2,B3,O = 800/800/800/29070, `CHECKSUM TABLE` 3947518761: **identical to the recorded state**; batches 4 and 5 absent (replay over three files) |
| Gap: data object of file 4 deleted from the bucket (manifest kept) | **Finding (product defect, fixed):** `pitr-proposal` still said `covered true` for times in files 4 and 5 and the restore failed late with `restore job failed: backup: object not found` and left a job service at 0/1 behind (known open issue). Cause: `ListBinlogArchives` read only the manifests. |
| Same after the fix (new manager binary) | proposal for batch 4 and 5: `covered false`, `reason "sequence 4 is missing after 3"`, `coverage.gap` the same; times in files 1-3 stay `covered true`; `POST /restores/inspect` for batch 4: **409**, no operation |

The fix (`internal/dbbackup/pitr.go`): a manifest whose stream object is missing from the listing is left out, so the chain stops with the existing gap text; test `TestListBinlogArchivesLeavesOutDeletedStream`. It costs no extra request (the listing already holds the names). It does not detect a stream that is present but truncated or altered; that is still caught by the digest check at restore time (`TestBinlogTamperingIsRejected`).

Not done in part 2: a gap from a **missing manifest** (the existing "sequence N missing" case is unit-tested; the lab only covered a deleted stream), a source node replaced after the backup, a loaded cluster, GTID continuation across nodes, the schedule form of the UI (policy set through the API; the form is covered by vitest), the retention interplay over days.

## Part 3: fresh-server bootstrap through the real HTTP API and the UI wizard

Layout: m1 a fresh Debian 12 (arm64, no Docker, no agent, root login with a lab-own key prepared by the harness), m2 the real manager (managed mode, auth, `manager.serverBootstrap` enabled with `agentRelease` pinned by SHA-256, agent binary served over HTTPS with a lab-own CA that the harness installed in m1's trust store), a single-node Swarm in m2. Administrator through the UI.

| Run | Path | Result |
| --- | --- | --- |
| A. Profile Docker + native agent, no cluster, host ID `fresh1` | API: `POST /bootstrap/drafts` (201, host key read without login), `/fingerprint`, `/plan`, `/approve`, `/run` | plan 5 steps, not blocked; `base-packages`, `service-user`, `sshd-audit`, `runtime-docker` **succeeded** (Docker 29.9.2 installed from `download.docker.com`); `agent-native` **failed: `provision: enrollment token unavailable`**, draft `paused`. Resume repeats the same failure. The host was changed (Docker, user `dbcm`) before the gap shows. |
| B. Profile Docker + Swarm `join-worker`, host ID `fresh-ui` | **UI wizard** (`/bootstrap`: form, host key, fingerprint typed, plan, approve, run) | completed in 5 s; m1 joined the Swarm; the draft's credentials were wiped (`credentialsHeld false`) |
| C. After B: Sync and Enroll m1 in the manager, then bootstrap m1 again with host ID = its Swarm node ID and the native agent | API | **409 / draft `blocked`: `provision: host already belongs to a cluster`**. A joined node is never planned. |
| D. Harness-only: `docker swarm leave` on m1, then a draft whose host ID is the node ID (host record still enrolled in the manager) | API | plan with `docker-existing` warning, completed in 5 s; `dbcm-patch-agent.service` active (report mode); the manager logged `enroll: agent enrolled host=<node id>`. This shows the native agent step itself works on a real Debian 12 over HTTPS with the pinned hash; it is **not** a supported path (it needs the stale host record). |

### The enrollment-token gap (documented, not decided)

What happens, precisely: `agent-native` is planned before the cluster step, and its enrollment token is issued by `HostEnrollmentTokens.InstallToken` only when the manager already holds an **enrolled** host record whose node ID equals the draft's `host.id`. A fresh server has no Swarm node ID and no record. Consequences observed: (1) the native agent can never succeed in the same bootstrap as the Swarm join; the failure appears only after Docker was installed and the user created, so the host is left half-prepared with the draft `paused`; (2) joining first does not help, because the join makes the bootstrap refuse the host (C); (3) the only way to a native agent after the Swarm join is the OS patch installer (E17 part c) or a host record that exists before the join (not creatable for a not-yet-known node). The container agent (`containerAgent`, a manager action after the Swarm join) was not run. Possible decisions (not taken): issue tokens for a host ID the operator names before any record exists; reorder the steps so the agent step runs after the cluster step and the manager enrolls the node by its join; or let the plan block `nativeAgent` without an enrolled record with a clear finding before anything runs. Guide 27 already names the limit; this run confirms it on a real host.

Not done in part 3: Ubuntu, Rocky, Alpine, Debian 13; lockdown (dedicated key, root login) through the API; the container agent step; `join-manager`; patch/restic capabilities through the bootstrap; password login; a manager restart or failover during a run; the UI wizard for the native agent path (only the API ran A, C, D; the UI ran B).

## Other observations

- After a failed point-in-time restore (gap case) the `dbcm-job-...-pitr-...` service stays at 0/1 (the open issue from E14 and E16: a dead job service is not removed by the operation).
- The login of the virtual passkey needs a growing signature counter across browser sessions; the driver bumps it per session (manager answers `login failed` otherwise). Not a product defect.
- The archive schedule kept running while I worked: 33 archive files had accumulated after one hour (`coverage.segments 33` until 08:20:30Z), with no thinning or size warning.

## Limits of this evidence

Single runs, synthetic data (30,000 rows), arm64 only, one MinIO build, no production data. Restores always go to a new isolated instance. Timing figures depend on the foreign host load.
