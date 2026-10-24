# E4 re-run, Part A (attempt 3): finished with exit 1, assertions failed in `failcreate` and `cleanuprestart`

Date: 2026-10-24, log UTC 03:31Z-03:54:54Z. Branch `worktree-agent-ac87453f465046d7d`, base `6027d74`. Raw log: [`e4-rerun-A3.log`](e4-rerun-A3.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh), library [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh).

## Invocation

- Binaries built with `go build -o bin/ ./cmd/...`.
- `bin/lab status` before the run: manifest empty; inventory and unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`. Host: 14 logical CPUs, 48.0 GiB RAM.
- `R_PHASES` set to the 19 Part A step names, `R_LOG` set to the A3 log. No product code and no lab tooling changed.
- Part A ran to completion (all 19 steps reached). The script exited 1 because of two recorded assertion failures.
- Uptime (`uptime` sampled every 60 s by a background loop, not by the script): 03:32:12Z load 26.95 / 67.24 / 91.73; 03:53:12Z load 20.60 / 34.82 / 49.96; 03:55:12Z load 23.00 / 29.15 / 45.60. Sampler output is outside the repo (scratchpad). The script does not log uptime per phase, so per-phase uptime is not recorded beyond these samples. Timings are unreliable under foreign host load, as the owner stated. No foreign process was touched.

## Per-step outcomes

| Step | Outcome | Evidence (log line) |
|---|---|---|
| build | passed | `## Phase build` (line 2) |
| vms | passed | `## Phase vms` (line 41); VMs `dbcm-lab-m1..m3` (lab apply, see log) |
| swarm | passed | `## Phase swarm` (line 204) |
| images | passed | `## Phase images` (line 252) |
| prereqs | completed, no assertion logged | `## Phase prereqs` (line 391) |
| rprep | completed, no assertion logged | `## Phase rprep` (line 462) |
| backend | completed, no assertion logged | `## Phase backend` (line 560) |
| manager | completed, no assertion logged | `## Phase manager` (line 607) |
| setup | completed, no assertion logged | `## Phase setup` (line 691) |
| swarmapi | completed; agent service created and labeled on all three hosts | line 732-840; `{"created":true,"labeled":[3 node ids],"unlabeled":[],"failed":[]}` |
| rcluster | passed | `03:36:39Z RESULT: create of e4db succeeded` (line 881). Step `05-exec-sql-n1` retried with `db-not-running` (attempts up to 3 seen in the monitor); step `19-await-update-n1` retried with `update-pending` until the update finished. |
| verify | completed; MariaDB evidence observed | line 892-1061: `wsrep_cluster_size 3`, `Synced`, `Primary` on all sampled nodes; the script's own checks did not log an assertion failure |
| rfailure | completed; node-outage events observed | line 1153 `03:40:45Z RESULT: node-outage events naming n3: 3`; monitor event at 05:37:07+02:00 "node n3 of cluster e4db is healthy again" |
| rbackup | passed | line 1845 `03:41:01Z RESULT: manifest records a Galera position: {"state_uuid":"ed03df71-cf5b-11f1-a442-0a505b38b212","seqno":18,"source":"stream"}` |
| rinspect | passed | line 1881 `03:41:11Z RESULT: inspection operation on the worker host ended: succeeded` |
| capacity | passed | line 2002 `03:45:14Z RESULT: operations 4 -> 4, clusters 1 -> 1 after the refused plans` |
| failcreate | **assertion failed** | line 2125-2127 `ASSERTION FAILED (failcreate): create ended failed after the abort, expected aborted`. The abort did take effect: the operation finished at 03:48:17Z with `state":"failed"`, `reason":"aborted at a safe boundary"`, `attempts":11` on step `05-exec-sql-n1`. Re-create of the same name refused: `{"error":"cluster_exists"}` (line 2145). Delete of the failed create: `succeeded` (line 2192). Create of the reused name `e4fail`: `succeeded` at 03:50:05Z (line 2243). |
| cleanuprestart | **assertion failed** | line 2346-2349 `ASSERTION FAILED (cleanuprestart): deletion across the restart ended failed`. Manager was SIGKILLed and came back (`wait ok after 2s: manager leads again`, 03:50:16Z). The delete op `op-b30b6c3d2f0f5b7b` ended `failed` at step `04-remove-volume-n1-26a244a2` after 3 of 6 steps: `failed (volume-in-use): ... HTTP 409: volume is in use`. |
| diskfull | completed; recovery observed; no assertion line logged | line 2424 `## Phase diskfull`. Node n2 on m3 failed with `task: non-zero exit (133)` while the volume filled; cluster `degraded` from 03:51:05Z; after freeing space: 03:54:24Z `n3 degraded/donor`, 03:54:39Z `state":"healthy"`, all nodes `healthy/synced`. |
| sentinels | passed | line 2570 `03:54:40Z RESULT: sentinel objects unchanged` |
| deleteall | passed | line 2598 `03:54:50Z RESULT: deletion of e4db ended: succeeded` |

## Error details

- `failcreate`: the abort reached the operation after the retries; the operation ended `failed` with reason `aborted at a safe boundary`. The script's expectation was `aborted`. This is a product-level finding for the owner: the operation's state after an aborted create is `failed` (reason `aborted at a safe boundary`), not `aborted`. The script assertion is not changed here. The A2 finding (abort did not finish the operation) did not reproduce in this run.
- `cleanuprestart`: the deletion did not complete across the manager SIGKILL and agent kill. The volume `dbcm-e4fail-n1-26a244a2` was still in use when removal was retried (`HTTP 409 volume is in use`), and the operation ended `failed`. Whether the agent kill left a container holding the volume, or the retry was too early, is not established from the log. Not fixed here.
- `diskfull`: no assertion is logged for this phase. The recovery observed is evidence only.
- Part B: see section below.

## Scoped cleanup and proof

- The script's exit trap ran `bin/lab cleanup -apply` at 03:54:54Z: `limactl stop --force` and `limactl delete --force` on `dbcm-lab-m1`, `m2`, `m3`. Post-snapshot diff: `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `added: (none)`, `unrelated resources intact (3)` (log section `## Scoped cleanup`).
- Explicit `bin/lab cleanup -apply` run afterwards: `pre-snapshot` lists the three unrelated resources, `executing actions: (none)`, `post-snapshot diff: added (none), removed (none)`, `unrelated resources intact (3)`.
- `bin/lab status` after the run: manifest empty; inventory and unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`. Identical to the baseline.
- `limactl list`: `thermoctl-e2e-basestation` Stopped (2 CPU, 3 GiB, 8 GiB), `thermoctl-test-vm` Stopped (4 CPU, 4 GiB, 20 GiB). No `dbcm-lab-*` entries.
- `colima:default` was not queried directly by `limactl`; it is unchanged in `bin/lab status`.
- Host free disk after cleanup: 37 Gi (not a measure of reclaimed space, see `docs/TESTING.md`).

## Open items

- Owner decision: the abort end state of a retrying create is `failed` with reason `aborted at a safe boundary`; the script expects `aborted`. Either the product state or the script expectation needs a decision.
- Owner decision / investigation: why the volume was still in use after the manager SIGKILL and agent kill in `cleanuprestart`.
- Part B: see `e4-rerun-B.md` if written in this run.
