# E4 re-run, Part A: failed in phase `vms` (lab VM start)

Date: 2026-10-23 (UTC 13:10-13:22). Branch `worktree-agent-ab20d00dd025310a4`, base `81e0a24`. Raw log: [`e4-rerun-A.log`](e4-rerun-A.log). Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh) (library [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh)).

## Invocation deviations

- `R_PHASES=A` was given as instructed, but `A` is not a phase name. `step()` matches individual names (`build`, `vms`, ...), so `A` skipped every phase and only the scoped cleanup ran (exit 0). That run's log was overwritten by the real run.
- The script is not executable (`-rw-r--r--`), so the first launch failed with exit 126 before any step. It was retried once with `bash tests/p0/e4-rerun.sh`. The mode was not changed.
- The real run used `R_PHASES="build vms swarm images prereqs rprep backend manager setup swarmapi rcluster verify rfailure rbackup rinspect capacity failcreate cleanuprestart diskfull sentinels deleteall"`, which is the `A_STEPS` list from the script.

## Host load and timing

Timings are unreliable: the host was under foreign load for the whole session, as the owner stated. Uptime samples:

- 13:10:29Z (15:10 local): load 878.71 / 795.30 / 925.54
- 13:11:17Z (vms start): load 674.66 / 903.48 / 953.16
- 13:22:31Z (after failure): load 999.32 / 954.00 / 949.56
- 13:22:42Z (after cleanup): load 936.91 / 942.33 / 945.48

No foreign process was touched. The run did not wait for load to drop.

## Per-step outcomes

| Step | Outcome | Evidence |
|---|---|---|
| build (`go build` linux/arm64: manager, agent, dbcm-job, e4client) | passed | four `(exit 0)` blocks, revision `81e0a24` |
| vms (`bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2`) | **failed** | `limactl start dbcm-lab-m1` exit 1 (see below); pre-snapshot listed only the 3 unrelated entries |
| swarm, images, prereqs, rprep, backend, manager, setup, swarmapi, rcluster, verify, rfailure, rbackup, rinspect, capacity, failcreate, cleanuprestart, diskfull, sentinels, deleteall | not reached | the script stopped at the failed `vms` phase |
| Part B | not run (Part A did not pass) | per instructions |

### Failing step: `vms`

- `limactl create --name=dbcm-lab-m1 --cpus=2 --memory=4 --disk=20 --tty=false --network=lima:user-v2 template:docker` ran first.
- `limactl start --tty=false dbcm-lab-m1` failed with:

```
time="2026-10-23T15:11:20+02:00" level=info msg="Using the existing instance `dbcm-lab-m1`"
time="2026-10-23T15:11:28+02:00" level=info msg="[hostagent] [VZ] - vm state change: running"
time="2026-10-23T15:11:47+02:00" level=info msg="[hostagent] Guest agent is running"
time="2026-10-23T15:12:32+02:00" ... "Time sync: guest clock adjusted" (repeated until 15:20:33)
time="2026-10-23T15:21:20+02:00" level=fatal msg="did not receive an event with the `running` status"
lab: start Lima VM dbcm-lab-m1: lab: limactl start --tty=false dbcm-lab-m1: exit status 1
```

Interpretation (not verified further): the VM reached the VZ running state and the guest agent and SSH came up by 15:11:47, but Lima's hostagent never received the `running` event and gave up after about 10 minutes. Between 15:12 and 15:20 the log shows repeated guest clock corrections of up to about 5 s, which fits host scheduler starvation under the load of about 900-1000. This is a hypothesis from the log, not a confirmed root cause.

- `m2` and `m3` were not created (the post-snapshot shows only `lima:dbcm-lab-m1`).
- The VMs were never reached by the manager, so no manager or agent log lines exist for this run.

## Scoped cleanup

`bin/lab cleanup -apply` (run by the script's exit trap):

- Actions: `limactl stop --force dbcm-lab-m1`, `limactl delete --force dbcm-lab-m1`.
- Post-snapshot diff: `removed: lima:dbcm-lab-m1`, `added: (none)`.
- `unrelated resources intact (3)`, exit 0.

Follow-up cleanup proof (run after the script exited):

- `bin/lab status`: manifest `resources: []`; inventory and unrelated entries `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`.
- `limactl list`: `thermoctl-e2e-basestation` Stopped (2 CPU, 3 GiB, 8 GiB, `127.0.0.1:55325`); `thermoctl-test-vm` Stopped (4 CPU, 4 GiB, 20 GiB, `127.0.0.1:50429`). Identical to the baseline before the run. `colima:default` was not queried by `limactl` and is unchanged in `bin/lab status`.
- Host free disk after cleanup: 45 Gi (9.6 Gi before the apply). Not a measure of reclaimed space, see `docs/TESTING.md`.

## Open items

- Re-run Part A when the host allows a VM start. Consider a longer `limactl start` timeout or a retry for `dbcm-lab-m1` only if the owner approves a change to the lab tooling. The product was not changed.
- The `R_PHASES=A` alias and the non-executable script are script/usage issues; they were not fixed in this session.
- Part B has not been run.
