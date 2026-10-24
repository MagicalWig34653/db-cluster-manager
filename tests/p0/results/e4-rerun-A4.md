# E4 re-run, Part A (attempt 4): all 19 steps passed, exit 0

Date: 2026-10-24, log UTC 04:24Z-04:46:59Z. Branch `worktree-agent-af405ad109074ceb8`, product code at `deeae3c` (the failcreate state and cleanuprestart retry fixes of this branch). Raw log: [`e4-rerun-A4.log`](e4-rerun-A4.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh), library [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh).

## Invocation

- Binaries built with `go build -o bin/ ./cmd/...`; the script's build phase cross-compiled the linux/arm64 binaries.
- `bin/lab status` before the run: manifest empty; unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`. Host: 14 logical CPUs, 48.0 GiB RAM; lab budget 7 CPUs, 16 GiB, 100 GiB. VMs: 3 x (2 CPU, 4 GiB, 20 GiB).
- `R_PHASES` = the 19 Part A steps, `R_LOG` = the A4 log. Fresh VMs.
- Host load (1-minute load average from the script's per-phase `host uptime` lines): 125-165 during the whole run (foreign: Spotlight and File Provider indexing were the top CPU consumers at the start). Timings are unreliable. No foreign process was touched.

## Per-step outcomes

| Step | Outcome | Evidence (log line) |
|---|---|---|
| build, vms, swarm, images, prereqs, rprep, backend, manager, setup, swarmapi | completed | section headers lines 2-732 |
| rcluster | passed | line 881 `RESULT: create of e4db succeeded` |
| verify | completed | `wsrep_cluster_size 3` on all sampled nodes (lines 902-1025) |
| rfailure | passed | line 1155 `node-outage events naming n3: 3`; cluster healthy again at 04:29:16Z (line 1078) |
| rbackup | passed | line 1847 `manifest records a Galera position: {... "seqno":18,"source":"stream"}` |
| rinspect | passed | line 1883 `inspection operation on the worker host ended: succeeded` |
| capacity | passed | line 2004 `operations 4 -> 4, clusters 1 -> 1 after the refused plans` |
| failcreate | **passed** (A3: assertion failed) | abort at 04:40:21Z; the operation ended at once in `state":"aborted"`, reason `aborted at a safe boundary`, 11 attempts on `05-exec-sql-n1` (lines 2117-2133). No assertion failure. Re-create of the same name refused `cluster_exists` (line 2144); deletion of the aborted create `succeeded` (line 2191); create of the reused name `succeeded` (line 2242). |
| cleanuprestart | **passed** (A3: assertion failed) | At the kill (04:42:11Z) the deletion was at `3/6 04-remove-volume-n1-1c3cbcfa remove-volume failed att=2 ... failed (volume-in-use): ... HTTP 409: ... volume is in use`, with the two task containers of n1 still listed on m3 although the services were removed (the A3 situation). Manager SIGKILLed, agent on m3 killed, manager restarted: `leadership started term=3 adopted=1`, then `operation succeeded op=op-d4a724a568b54020 kind=cluster/delete` (manager log section after line 2346); result line 2345 `deletion across a manager SIGKILL and an agent kill ended: succeeded`. Afterwards only the non-owned decoy volumes `dbcm-e4fail-n1-decoy` remain. |
| diskfull | completed; recovery observed (no assertion in this phase) | n1 on m3 `task: non-zero exit (133)` while the volume filled; cluster `degraded` from 04:43Z; after freeing space `healthy`, all nodes `healthy/synced` at 04:46:44Z |
| sentinels | passed | line 2573 `sentinel objects unchanged` |
| deleteall | passed | line 2601 `deletion of e4db ended: succeeded` |

## What the two fixed findings show

- failcreate: the API now reports an operator abort as `aborted` (A3 reported `failed` with the same reason). The cause was the API state mapping (`runner.StatusAborted` mapped to `failed`), not the runner; the fakes tests had asserted the runner status only.
- cleanuprestart: the volume step had failed twice with `volume-in-use` and was in its retry backoff at the kill. The new leader found the retry journaled as due and retried it; the run does not record how many further attempts it needed (the operation finished 1 s after the restart). The log shows only one run of this case; the restart timing relative to the backoff is not controlled by the script.

## Scoped cleanup and proof

- Exit trap `bin/lab cleanup -apply` at 04:46:59Z: stop and delete of `dbcm-lab-m1..m3`; post-snapshot diff `added: (none)`, `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `unrelated resources intact (3)`.
- `bin/lab status` after cleanup: manifest empty; inventory `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`, identical to the baseline.
