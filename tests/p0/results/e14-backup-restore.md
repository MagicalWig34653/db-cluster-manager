# E14: stage 3 (backup, restore, copy, failure cases) on lab VMs, 2026-10-30

Three Lima VMs (arm64, 2 vCPU / 4 GiB / 20 GiB, Docker 29.9.2, MariaDB 11.4.13) created with the E12 phases `build vms swarm` ([e14-vms.log](e14-vms.log)). Manager from `deploy/Dockerfile` built on m1 from the committed tree (`git archive`), single-instance stack `deploy/stack.yaml` (modified only by uncommenting the backend secret blocks and adding a copy-target secret). MinIO (pinned module build from E11) with a self-signed certificate on m2 as the S3 target; a `local` target inside the manager volume for copies. Driven by Playwright/Chromium with a virtual WebAuthn authenticator (UI) and the browser session for API calls. Scripts were run from a scratch directory and are not part of the repository; no secrets were recorded.

## Executed and passed

| Step | How | Result |
| --- | --- | --- |
| Install per `docs/INSTALL.md` | `manager init -backup`, secrets, config, `docker stack deploy` | Manager up, setup token read from the service log; gaps below |
| Administrator, Swarm, enroll 3 hosts, deploy agents, 3-node Galera `shop` | UI | operation succeeded (23/23 steps) |
| Test data | SQL on n1 | `app.orders` 200,000 rows, `app.audit` 1,000 rows; `CHECKSUM TABLE` 3137479944 / 375525613 |
| Backup now | UI button on Backups page | succeeded in about 3 s; stream (about 125 MiB), manifest and status in the MinIO bucket |
| Inspection restore on m3 | UI: select, run, approve digest | operation 11/11; restored instance `wsrep_on OFF`, **200,000 and 1,000 rows, identical CHECKSUM TABLE values**; `lastVerifiedRestoreAt` set |
| Copy to second target | `POST /clusters/{id}/backup-copies` to `local` | succeeded; SHA-256 of the copy equals the manifest digest and the stream reassembled from MinIO |
| Restore proposal from the copy | `POST /backup-copies/{id}/restore-proposal` | 200 with digest, `approvalRequired true`, `autoPromotion false` |
| Schedule and retention | `*/4` schedule, `keepLast 2`, UI form and API | scheduled backups ran every 4 min; retention deleted older sets and kept the newest verified one; events "retention deleted 1 backup set(s)" |
| Scheduled restore verification | `verify` every 5 min with the node ID | passed on a real inspection instance; `lastVerifiedRestoreAt` advanced; with the host **name** it failed with the event "host ... is not enrolled in the cluster's Swarm" |
| Wrong credentials | MinIO root password changed | the job fails; the operation waits with `s3 status 403 SignatureDoesNotMatch` as reason |
| Unreachable target | MinIO stopped before a backup | job exits (5 retries), operation waits at `wait-backup` with a connection-refused reason, later `manifest not written yet` |
| Full target | 40 MiB tmpfs under MinIO | reason `backup job failed: ... 507 XMinioStorageFull` |
| Login limiter | 7 unfinished `login/begin` calls, then the UI | 429 with `Retry-After: 900`; UI now shows "locked for about 15 minutes" ([screenshot](../../../docs/assets/screenshots/real/real-login-429.png)) |

## Bugs found and fixed (test first where possible)

1. Corrupt copy reported as copied: after one byte of a copy was flipped, a second copy request succeeded in milliseconds without rewriting it (shallow size check). `CopyBackup` now verifies the existing copy deeply and rewrites it; the restore proposal had already refused the corrupt copy (409). Test `TestCopyBackupRepairsCorruptExistingCopy`.
2. Retention left one `.status.json` per deleted set in the bucket (14 orphans observed). `ApplyRetention` removes it last; test extended.
3. From E13 (found earlier, fixed in commit 0ef462d): a successful sign-in no longer counts toward the `login/begin` limiter; abort of a never-started (`unknown`) operation closes the record instead of 404; abort is refused with 409 while a rolling restart, upgrade or scale operation has stopped a node and not started it again (so no node stays at 0 replicas). The limiter 429 message names the wait time.

## Findings not fixed (documented)

- Follow-up 2026-10-31 (fakes): abort now removes the job services, `verify.host` accepts the host name, the failed-verification event no longer prints an empty backup id. Still open: a dead job service (target unreachable) is invisible to the operation, which polls forever; abort leaves the `dbcm-job-*` service at 0/1 behind (removed by hand with its exact name). [Troubleshooting](../../../docs/guides/en/09-troubleshooting.md).
- `verify.host` takes the Swarm node ID; the host name is rejected at run time (documented in guide 21). The failed-verification event text has an empty backup id.
- INSTALL.md gaps (fixed in the document): database image must be on every node; `init` from the container needs `--user` and a writable mount; `-origin` for localhost; the whole backup setup (shared backup password, backend JSON, bucket, both `secrets:` blocks of the stack file, copy targets).

## Not run

- **PITR to a chosen timestamp**: no API or UI route existed (binlog archiving and planning were library code with fakes; the cluster ran without `log_bin`). Not attempted in E14; implemented on 2026-10-31 and run with the real job helper in [E15](e15-pitr.md).
- Execution of a restore from a copy into a **new target cluster** through the API: the endpoint ends at the proposal (documented limit). Equivalent evidence is the inspection restore with identical checksums.
- Interrupted backup in mid-stream (the 125 MiB stream completes in about 2 s, so a mid-stream stop could not be timed); covered by the unreachable-target case above.
- Fresh-VM walkthrough with a separate reader: I followed INSTALL.md on freshly created VMs myself (above), but the installer and the gap finder are the same person with project knowledge, and the German guides were only skimmed.
