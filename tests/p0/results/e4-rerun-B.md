# E4 re-run, Part B (three manager replicas): finished with exit 1, failed in `hamanager`

Date: 2026-10-24, log UTC 03:58Z-04:00:39Z. Branch `worktree-agent-ac87453f465046d7d`. Raw log: [`e4-rerun-B.log`](e4-rerun-B.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh), library [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh).

## Invocation

- Fresh run after Part A (`bin/lab cleanup -apply` had already removed the Part A VMs; unrelated resources intact).
- `R_PHASES` set to the setup steps (`build vms swarm images prereqs rprep backend manager setup swarmapi`) plus the Part B steps (`hareset hamanager hasetup haswarm hacreate hafailover hadelete`). `R_LOG` set to the B log. `hareset` needs the Swarm and agents that the setup steps create, so a fresh setup was required.
- Uptime: not recorded per phase by the script. The background sampler stopped with the shell session; no uptime sample for Part B exists.

## Per-step outcomes

| Step | Outcome | Evidence (log line) |
|---|---|---|
| build, vms, swarm, images, prereqs, rprep, backend, manager, setup, swarmapi | completed; no assertion logged | section headers in the B log (lines 2-732); swarmapi `created`/`labeled` on three hosts (line 732-832, see log) |
| hareset | completed; promotion observed | line 840; `Node lima-dbcm-lab-m2 promoted to a manager in the swarm.`, `Node lima-dbcm-lab-m3 promoted`; `docker node ls`: m1 Leader, m2 Reachable, m3 Reachable (all Ready, Active) |
| hamanager | **failed** | line 923 section start; replicas configured on m1, m2, m3 (`bootstrap: true`, `autoResume: true`, peers 192.168.104.1, .3, .4 on port 18490); `start replica` on m1, m2, m3 at 03:58:36-03:58:37Z; then `04:00:37Z wait TIMEOUT after 120s: a replica leads` (line 1150-1215 area). The script exited 1 here (`set -e`), so no further steps ran. |
| hasetup | not reached | - |
| haswarm | not reached | - |
| hacreate | not reached | - |
| hafailover | not reached | - |
| hadelete | not reached | - |

## Error details

- `hamanager`: no replica was observed to lead within the script's 120 s wait. The replica logs (`journalctl -u dbcm-e4-manager` on each VM) are not captured in the run log, and the VMs were deleted by the script's exit trap, so the reason is not established. Candidates from the log alone: the replicas' metastore bootstrap did not form a quorum (peers were given with `bootstrap: true` on all three), or the wait's check is not satisfied by the replicas' status output. Not diagnosed further here; the product was not changed.
- Part B did not pass. No Part B step after `hamanager` ran.

## Scoped cleanup and proof

- The script's exit trap ran `bin/lab cleanup -apply` at 04:00:39Z: `limactl stop --force` and `limactl delete --force` on `dbcm-lab-m1`, `m2`, `m3`. Post-snapshot diff: `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `added: (none)`, `unrelated resources intact (3)` (log section `## Scoped cleanup`).
- `bin/lab status` after cleanup: manifest empty; inventory and unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`. Identical to the baseline.
- `limactl list`: `thermoctl-e2e-basestation` Stopped (2 CPU, 3 GiB, 8 GiB), `thermoctl-test-vm` Stopped (4 CPU, 4 GiB, 20 GiB). No `dbcm-lab-*` entries.
- Host free disk after cleanup: 37 Gi.

## Open items

- `hamanager` failure: the quorum/leader wait timed out. A follow-up run needs the replica logs captured before cleanup (for example, a `journalctl` dump in the wait's failure path). Owner or follow-up decision.
- `hasetup`, `haswarm`, `hacreate`, `hafailover`, `hadelete` are not verified.
