# E4 re-run: not executed (host load)

Date: 2026-10-23. Script: [`tests/p0/e4-rerun.sh`](../e4-rerun.sh), committed but **not executed**. No lab VM was created in this session, so this file has no lab results. It records why the run did not start, what the script covers, and the open items.

## Why the run did not start

The task allows a lab session only when the host has room for it; with heavy foreign load the run has to wait. Foreign processes were never touched.

- The lab budget was available: `bin/lab status` showed 14 logical CPUs (lab limit 7), 48 GiB RAM (lab limit 16 GiB), an empty manifest, and the unrelated resources `colima:default` (running, 6 CPUs / 8 GiB, foreign) and two stopped `thermoctl-*` Lima VMs. Host free disk was 60 GiB (`df`, volume 94 % full).
- The host load came from unrelated user work: Xcode builds (`swift-frontend`), `xctest`, iOS simulator UI tests, `fileproviderd`/`cloudd`/`bird` (iCloud) and `fseventsd`.
- `vm.loadavg` samples (1-min / 5-min) during the wait, about 4.5 h until 13:03Z: 110/66, 93/82, 379/236, 418/256, 454/286, 85/171, 30/46, 127/160, 253/185, 148/167, 86/100, 344/246, 310/246, 176/197, 390/321, 357/378, 342/286, 199/209, 163/249, 78/145, 35/18 (one short dip), 838/535, 417/479, 324/402, 229/329, 794/758, 971/1010.
- Start threshold: first 1-min < 14 and 5-min < 20 (the clean E4 runs started at 4-15). After about 2.5 h it was raised to 1-min < 30 and 5-min < 40. Neither was reached for the length of a run. The only dip (5-min 18) lasted less than a minute before the next sample went over 800.

Under similar load (up to about 800) E3 saw slow elections and a stale leader, and the E4 development run (load 13-124) produced timings that could not be trusted. The re-run measures leader failover, the restart of an operation and Galera behaviour while a disk is full. Results gathered at load 300-1000 could not be told apart from scheduler starvation, so the session stopped without creating VMs.

Cleanup proof: nothing was created. At the start and at the end, `limactl list` showed only `thermoctl-e2e-basestation` and `thermoctl-test-vm` (both stopped), `colima list` showed `default` (foreign) and `.lab/manifest.json` did not exist (empty manifest). No `dbcm-lab-` VM, no Docker context and no Colima profile was created, changed or deleted.

## What the script covers (ready to run)

`tests/p0/e4-rerun.sh` sources the E4 script as a library. It reuses its VMs, Swarm, images, prerequisites, SFTP backend, manager, setup and agent phases. The manager runs with `autoResume: true` (new variable `E4_AUTORESUME` in the E4 script; the default stays `false`).

Part A, E4 topology (m1 Swarm manager, m2/m3 workers, one manager process):

| Phase | Checks |
| --- | --- |
| `rprep` | 2.5 GiB file-backed ext4 mounted at the volume base on m3 before the agent mounts it. Non-owned sentinels: a service without an installation label, a labelled volume per host, a decoy volume `dbcm-e4fail-n1-decoy` carrying another installation's label, and a file directory inside the volume base. |
| `rcluster` | create `e4db` and sample the operation's `progress` (steps, attempts, polls, waiting reason) every 5 s |
| `rfailure` | node kill (E4 phase), then `GET /events` and `GET /notifications`; counts the node-outage events |
| `rbackup` | physical backup to SFTP; reads the newest manifest's `galera` field and `warnings`. This confirms or refutes that MariaDB 11.4 puts `galera_info` into the xbstream. |
| `rinspect` | inspection restore with `host` = the worker m2 and a TTL of 4 min: approval, wait for `succeeded` through the `restore-verified` gate, a query with the one-time account on m2, `lastVerifiedRestoreAt`, then the TTL teardown (instance service, init secret and restore volume gone) |
| `capacity` | 3 GiB per node next to `e4db`, 16 GiB per node and 5 nodes on 3 hosts: each must be refused with no operation, cluster record or service created |
| `failcreate` | create `e4fail` with an unpullable image digest: progress while it polls, abort, a second create under the same name (must be refused), `DELETE`, wrong digest (refused), approval, objects gone. Then `e4fail` is created again with the real image (the name can be reused). |
| `cleanuprestart` | `DELETE` of the healthy `e4fail`: after the first step it SIGKILLs the manager and kills the agent container on m3, restarts the manager and expects the deletion to resume and succeed, with `e4db` untouched |
| `diskfull` | fill the m3 volume filesystem to about 20 MiB free, write 40 × 1 MiB through another node, watch the API, events and node logs for 3 min, free the space, watch recovery for up to 6 min |
| `sentinels`, `deleteall` | compare the sentinel snapshots; delete `e4db` through the API |

Part B, three manager replicas: `hareset` stops the single manager, removes its agent service and enrollment secrets, clears the agent state and promotes m2/m3 to Swarm managers. `hamanager` starts three replicas with peers on user-v2, a shared 32-byte data key copied VM to VM through a host pipe (never stored on the host) and `bootstrap` on all replicas. Then `hasetup`, `haswarm`, and `hacreate`, which SIGKILLs the leader replica after 4 create steps and measures the time until another replica answers. It expects the create to resume with exactly three services and one volume per slot, and the old replica to rejoin as a follower. `hafailover` SIGKILLs the leader during a backup and runs 30 writes to the cluster while the leader is lost. `hadelete` deletes `e4ha`.

Static checks only: `bash -n` and `shellcheck -x -S warning` pass. Neither part has run, so their phase logic may still contain script defects.

## Three manager replicas: feasibility from the configuration

The manager supports several replicas: `manager.peers` (initial voting membership), `bind`/`advertise`, `bootstrap`, and a shared `keyFile`. There is **no membership change** (no add/remove voter), so a single-instance installation cannot grow into three replicas. The script therefore starts a second installation in part B. Every replica needs its own Docker endpoint on a Swarm manager node, because the Swarm record holds a `unix://` socket that the current leader dials locally; so m2/m3 are promoted. Other limits, from code reading and not observed:

- Non-leaders answer 503 with `X-DCM-Leader` and do not forward requests.
- Agents keep the `managerUrl` of the replica that deployed them.
- Metastore peer traffic has no TLS.
- The data key has to be copied out of band.

## amd64

All lab evidence so far is arm64 (Apple silicon, Lima `vz`). amd64 on this host would only be emulation (QEMU TCG). Emulation can show functional behaviour but is far too slow for Galera or Raft timing and cannot stand in for native evidence ([TESTING.md](../../../docs/TESTING.md)). A native amd64 run needs an amd64 machine: an x86 host or cloud VM that the owner provides, or a GitHub-hosted `ubuntu-latest` runner for single-host functional checks (no Swarm across hosts). This is not decided and not done.

## Open after this session

Everything in part A and part B, the redundant proxy/VIP evaluation, a VM power-loss outage, resource measurements per gate, and native amd64. The P0 exit gates in [RELEASES.md](../../../docs/RELEASES.md) are not met: there is no multi-replica manager evidence on VMs, no measured resource use per gate, and no evidence on a second architecture. W05 stays IN PROGRESS.
