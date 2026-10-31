# E16: install walkthrough from INSTALL.md and PITR through the real manager, 2026-10-31

Three Lima VMs (arm64, 2 vCPU / 4 GiB / 20 GiB, Docker 29.9.2, MariaDB 11.4.13) created with `bin/lab` through the E12 harness phases `build vms swarm` (VM creation and `docker swarm init/join` are the only harness use; everything after followed `docs/INSTALL.md` and the linked guides). Host before the run: load 7, 62 GiB free. `bin/lab cleanup --apply` ran at the end (3 VMs removed, the 3 unrelated Lima/Colima entries intact). Scripts (Playwright with a virtual WebAuthn authenticator, shell snippets run in the VMs) lived in a scratch directory and are not part of the repository; no secrets were recorded.

**Honest limit of the "fresh reader":** the person walking through had read the repository, E13/E14/E15 and the code beforehand, so this is not an independent novice. Gaps were collected where the text did not say what I had to do next or was wrong, but I may have skipped gaps that my background filled silently. VMs and Swarm came from the harness, MinIO from a prebuilt binary (the container registries `quay.io` and Docker Hub had no usable MinIO image).

## What ran

| Step | Result |
| --- | --- |
| Source on the node, `docker buildx build` of manager and job image, `docker save/load` to the other nodes, `mariadb:11.4.13` pulled everywhere | worked |
| `manager init -backup` from the image with `--user` and `-origin http://localhost:8080` | worked; output matches INSTALL |
| Host dirs, secrets, networks, shared backup password, backend JSON (S3, plain HTTP MinIO on m2, bucket created by directory), recovery key secret, Swarm config | worked |
| `docker stack deploy` of a copy of `deploy/stack.yaml` with both secrets blocks uncommented | worked after gap 3; manager 1/1, setup token in the log |
| Administrator through the real UI (token, passkey, recovery codes), Swarm connect, Sync, Enroll x3, Deploy agents | worked; three agents enrolled within seconds |
| 3-node Galera `shop` with **binlog=true** through the wizard | operation 23 of 23 in 2 min 1 s; `log_bin=1`, `binlog_format=ROW`, cluster size 3 |
| German UI (language switch, 11 pages) | works, findings below |

## PITR through the real manager (part A)

| Step | Result |
| --- | --- |
| Data O: `pitr.orders`, 2000 rows | written on n1 |
| "Start backup now" (UI) | succeeded in 3 s; stream, manifest and status objects in the bucket |
| Batch A (insert 1000, update, delete), wait, **T = 2026-10-31 06:47:41 UTC**, wait, batch B (insert 800, update, delete) | state at T read with `wsrep_sync_wait`: 2920 rows, sum(amount) 317008, sum(id) 13404420, batches A,O, `CHECKSUM TABLE` **1109488220**; final state 3572 rows, batches A,B,O, checksum 3763193701 |
| "Archive binary log now" (UI) | succeeded in 3 s |
| Restore wizard, "Point in time", T, "Check coverage" | `covered: true`, starting backup b20261031t064716-983dcf41, 1 segment, archive covers 06:47:17 to before 06:47:53 UTC (the run closed the open binlog file, so T before the run is covered) |
| Plan, acknowledgement, "Run inspection", one-time password, approval on the card | operation `dbbackup/inspection` 15 of 15 in 41 s |
| Comparison on the restored instance (`wsrep_on 0`) | **2920 rows, sum(amount) 317008, sum(id) 13404420, batches A,O, `CHECKSUM TABLE` 1109488220**: identical to the cluster at T; the later batch B is absent |

## Documentation gaps (INSTALL.md and guides, all fixed in this change unless noted)

1. No statement that the Swarm must already exist and how (`docker swarm init/join`); no mention of Buildx. Fixed ("Before you start").
2. No statement that the repository must be on the node (the commands use `deploy/Dockerfile*`, `deploy/stack.yaml`), nor that a release image does not exist so the checkout is needed. Fixed.
3. `docker stack deploy` with variables and `sudo` fails (`sudo` drops the variables: "required variable DBCM_CONFIG_NAME is missing"). The snippet shows no `sudo`. Fixed (`sudo env ...`).
4. The stack-file comment says the HA stack is "never run" and points at a section name that does not exist ("Three-replica variant"); the file comment for the secrets block names only the service-level block. Fixed in `deploy/stack.yaml`, the guides and INSTALL.
5. Uncommenting the two `secrets:` blocks: which lines and which indentation was left to the reader; the deploy then needs `DBCM_BACKEND_SECRET`. Fixed with exact indentation rules.
6. Backend JSON: only a TLS+CA example; a plain `http://` endpoint works. The endpoint must be an address reachable from every node. Bucket creation and the prefix without slash were not described. Fixed.
7. `init` output referred to "docs/INSTALL.md, \"Backups\""; the heading is "Backups: targets, secrets and stack changes". Fixed in the output.
8. The generic secret loop in the `init` output gave `dbcm-<inst>-backup-pw` a random value while the option file needs the same password; a reader following the output first would create the wrong secret. Fixed: with `-backup` the output omits the backup secret from the loop and prints the shared-password commands (test in `cmd/manager/init_test.go`).
9. SSH forward: on Lima the guest port is already forwarded; `ssh -L` then competes for the port. Fixed (note).
10. UI order of operations (Connect, **Sync**, Enroll, Deploy agents) was only described as API calls; recovery codes and "Continue to the manager". Fixed in guides 1 and 2 (EN, DE).
11. Cluster wizard: tag vs digest ("pinned by digest" reads as mandatory), the need for a CPU and memory limit, the binlog checkbox were not in guide 3. Fixed (EN, DE).
12. Guide 1 stated the install steps were [Untested] / HA stack "not shipped"; both outdated. Fixed.
13. PITR guide described the pieces but not the click path, the acknowledgement checkbox, the one-time password, how to connect to the restored instance, or that "Archive binary log now" closes the open file. Fixed (guide 5 EN, DE).
14. German guides used mock screenshots with a mock caption; replaced with real German screenshots (below) for the pages captured; the pages not captured (outage recovery, drift, keyring, services, registries, vulnerabilities, notifications) still use mock images.

## German UI check

Language switch works and persists across pages; all labels, hints and the wizard are German. Real screenshots of the German UI in `docs/assets/screenshots/real/`: `real-de-overview`, `-swarms`, `-hosts`, `-clusters`, `-cluster-detail`, `-wizard`, `-backups`, `-backup-policy`, `-operations`, used in the German guides 1, 2, 3, 5 and 6. Findings:

- **Fixed:** the last backup, last verified restore, next run and expiry time were all shown as "beobachtet vor ..." / "observed in 4 hours" (the freshness component always prefixed "observed"); a `plain` mode now shows the bare relative time (also image jobs, services, replicas, feed success time). The two German screenshots of the Backups and cluster detail pages were taken before this fix and still show the old wording (stated in their captions).
- **Fixed:** the Backups table column "Backup policy" showed the cluster's automatic-replacement policy ("manual"), untranslated, when no schedule exists; it now shows "No schedule (manual)". The cluster detail "Automation policy" shows translated "Manual replacement"/"Automatic replacement" instead of the raw API value.
- **Fixed:** editing the schedule in the UI silently dropped a `verify` policy that had been set through the API (the form did not carry it). It is now passed through unchanged (vitest).
- Not fixed: event texts and operation reasons come from the manager in English (known; the events list in the German UI is a mix); the sidebar does not extend over a full-page screenshot (artifact of the capture).

## Product findings

- Archive schedule in the backup policy (the optional extra): implemented. `PUT /clusters/{id}/backup-policy` accepts `archive {enabled, schedule, timezone}`, the leader's scheduler starts the same archive operation as the button, a refused run records a warning event, the UI form has a checkbox and a schedule field, OpenAPI and guides updated. Tests: Go (`TestArchivePolicyValidation`, `TestScheduledBinlogArchive`, which also guards that `RunSchedules` no longer drops `ArchiveLast`) and vitest. **Fakes only; not run on lab VMs** (the VMs were removed before the feature existed).
- `RunSchedules` rewrote the policy record from a field list and would have dropped new fields; it now copies the record.
- Observed, not investigated: at the start of a backup, an archive run and the inspection the cluster events show "node n1/n2 became unknown: ... connection refused" to the agent port 7443 for a few seconds, followed by "healthy again" (the agent service appears to be updated or restarted when a job needs secrets; `dbcm-agent-lab16` showed 1/3 once during cluster creation). Not caused by data loss; to be understood before a production trial.
- Observed: right after "Start backup now" the UI card showed "Pending / Paused: the manager paused this operation because a precondition is not met" for the first second although the operation then succeeded in 3 s. Wording is alarming for a normal first poll.
- The inspection instance was left to expire (4 h); the lab VMs were removed instead, so the expiry teardown was not observed here (E14 covered it).

## Not run

- The archive schedule on a real manager (fakes only), more than one archived file, a gap, a node replacement after the backup, a loaded cluster, production-size timing, GTID continuation across nodes.
- HTTPS/CA S3 endpoints (plain HTTP MinIO used), the `local` copy target, scheduled backups and retention (E14 ran them), amd64, the HA stack, a reader who has not seen the code.
- The German pages outage recovery, drift, keyring, services, registries, vulnerabilities, notifications were not captured.
