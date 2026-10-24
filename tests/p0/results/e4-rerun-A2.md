# E4 re-run, Part A (attempt 2): finished with exit 1, failed in `failcreate`

Date: 2026-10-23/24 (log UTC 22:0x-23:07:51Z). Branch `worktree-agent-ab4cfd4c51b3e8903`, base `7a075b7`. Raw log: [`e4-rerun-A2.log`](e4-rerun-A2.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh), library [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh).

## Invocation

- Binaries built with `go build -o bin/ ./cmd/...`, then `bash tests/p0/e4-rerun.sh` with `R_LOG` set to the A2 log and `R_PHASES` set to the 19 Part A step names from `A_STEPS`. No lab tooling was changed; no start timeout was added.
- Part B was not run (Part A did not pass).
- Host load: 541 to 990 during the run (uptime samples in the log). Timings are unreliable, as stated by the owner. No foreign process was touched.
- `bin/lab status` before the run: manifest empty, inventory and unrelated entries `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`.

## Per-step outcomes

| Step | Outcome | Evidence (log section / line) |
|---|---|---|
| build | passed | `## Phase build`, `go build` of manager, agent, dbcm-job, e4client (lines 2-40) |
| vms | passed | `bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20`, VMs `dbcm-lab-m1..m3` created and started (no `limactl start` timeout in this run); rootful Docker switched on each VM (lines 41-204) |
| swarm | passed | Swarm init on m1, join of m2 and m3, `docker node ls` (lines 204-252) |
| images | passed | agent and job images built on m1, copied to m2 and m3 via `docker save/load`, MariaDB pinned image pulled on all three (lines 252-391) |
| prereqs | passed | host directories, overlay and inspection networks, secrets created (lines 391-462) |
| rprep | passed | 2560M ext4 volume mounted on m3, sentinel service, sentinel and decoy volumes on m1, m2, m3 (lines 462-560) |
| backend | passed | SFTP backup target on m1 (lines 560-607) |
| manager | passed | `dbcm-e4-manager` started on m1 (lines 607-691) |
| setup | passed | first administrator via software passkey (lines 691-732) |
| swarmapi | passed | Swarm connect, sync, enroll of three hosts, agent service created and labeled on all three (lines 732-840) |
| rcluster | passed | `e4db` create operation `op-032550d091d1a11f` succeeded, 23/23 steps (line 939 `RESULT: create of e4db succeeded`). Step `05-exec-sql-n1` failed on attempts 1 to 8 before passing; steps `12-wait-synced-n2` and `17-wait-synced-n3` reported `node unreachable: agent answered refused/invalid-path: lstat /var/lib/dbcm/volumes/dbcm-e4db-n<k>-...` until the nodes came up. The retry-internal errors are not visible in the log; only the status string is. |
| verify | passed | all three nodes `wsrep_cluster_size 3`, `Synced`, `Primary`; writes on n1 and n3 visible on n1, n2, n3; monitor account over the node socket (lines 950-1117) |
| rfailure | completed without script abort (no explicit assertion in the script for this step; evidence only) | container of n3 killed on m1; `RESULT: node-outage events naming n3: 3`; nodes n2 and n3 reported healthy again at 00:49:56 +02:00 (lines 1119-1233) |
| rbackup | passed | `op-f1c13ab44da07e09` physical backup succeeded (`succeeded all steps succeeded`, 11/11); Galera position recorded in the manifest `{"seqno":19,"source":"stream"}` (lines 1235-1928) |
| rinspect | passed | inspection restore on worker m2, approval, `op-f1c13ab44da07e09` inspection operation succeeded; query on m2; secrets expired and teardown ran (lines 1930-2040) |
| capacity | passed | 3 GiB per node (`{"error":"no_capacity"}`, HTTP 409), 16 GiB per node (HTTP 409 no_capacity), 5 nodes on 3 hosts (HTTP 400 bad_request); no services created; `operations 4 -> 4, clusters 1 -> 1` (lines 2041-2092) |
| failcreate | **failed** | `e4fail` create (`op-552dafaae9ad028a`, unpullable digest `sha256:000...`) got stuck in step `05-exec-sql-n1` (exec-sql on worker `3d29feorxu7uuiimgt9x5rzyx`, attempts up to 21). `POST .../operations/op-552.../abort` returned HTTP 202, but the operation stayed `running`: `op-552dafaae9ad028a not finished after 300 s` (line 2186). Re-create of the same name was refused correctly (`HTTP 409 cluster_exists`). Deletion of `clu-fc22eed722de44d9` was refused (`HTTP 409 busy`) (lines 2095-2211). |
| cleanuprestart | not reached | script exited after failcreate |
| diskfull | not reached | - |
| sentinels | not reached | - |
| deleteall | not reached | - |

## Error details

- `failcreate`: the failing step was reported as `exec-sql` on the worker host, not as an image pull error. The unpullable image was expected to fail at pull, but the log does not show the pull error. The abort did not terminate the operation, and the delete of the cluster was refused as busy. This is a product-level finding for the owner: abort does not finish a create that is retrying an idempotent step. Not fixed here, per instructions.
- The exit cause is not logged directly. The last entries are the refused delete (`HTTP 409 busy`), then `deletion op  digest` with an empty operation id, then the exit trap. Most likely the script's `set -e` stopped at the failed delete. This is inferred from the log, not confirmed.
- Part A did not pass, so Part B was not run.

## Scoped cleanup and proof

- The script's exit trap ran `bin/lab cleanup -apply` at 23:07:51Z: `limactl stop --force` and `limactl delete --force` on `dbcm-lab-m1`, `m2`, `m3`. Post-snapshot diff: `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `added: (none)`, `unrelated resources intact (3)` (lines 2213-2247).
- Follow-up run of `bin/lab cleanup -apply` (after the script exited): `executing actions: (none)`, `removed: (none)`, `unrelated resources intact (3)`, exit 0.
- `bin/lab status` after cleanup: manifest empty; inventory and unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`. Identical to the baseline.
- `limactl list`: `thermoctl-e2e-basestation` Stopped (2 CPU, 3 GiB, 8 GiB), `thermoctl-test-vm` Stopped (4 CPU, 4 GiB, 20 GiB). Same as before the run. No `dbcm-lab-*` processes remained.
- `colima:default` was not queried directly by `limactl`; it is unchanged in `bin/lab status`.
- Host free disk after cleanup: 42 Gi (not a measure of reclaimed space, see `docs/TESTING.md`).

## Open items

- Owner decision: abort semantics for a create whose step keeps retrying (`failcreate`). The product was not changed.
- Re-run Part A from `failcreate` onward, plus `cleanuprestart`, `diskfull`, `sentinels`, `deleteall`, and then Part B.
- The `exec-sql` retry error text is not captured by the manager log or the agent logs (agent logs show startup lines only). Capturing it would need the operation's step error field, which the script does not record. Follow-up for the script.
