# E2: Host loss, returning host, empty-volume trap and partition

Date: 2026-10-19. Script: [`tests/p0/e2-host-loss.sh`](../e2-host-loss.sh). Guard: [`tests/p0/marker-guard.sh`](../marker-guard.sh). Shared setup: [`tests/p0/common.sh`](../common.sh). Full sanitized log of the recorded run: [`e2-host-loss.log`](e2-host-loss.log).

The figures below come from one complete run from scratch: `tests/p0/e2-host-loss.sh`, no environment overrides, exit 0, 14:30:33Z to 14:37:35Z (about 7 minutes including VM creation and cleanup). Two development runs came before it: run 1 with `E2_KEEP_VMS=1` was aborted in step 4, and run 2 with `E2_REUSE_VMS=1 E2_KEEP_VMS=1` passed. Their VMs were removed with `bin/lab cleanup -apply` before the recorded run. Their logs were overwritten. What run 1 revealed is described under "Failures and surprises".

All results are native arm64 (Apple Silicon host, `vz` VMs). The environment is the same as E1: `limactl version 2.2.0`, three VMs `dbcm-lab-m1..m3` with 2 vCPUs, 4 GiB RAM and 20 GiB disk each (6 CPUs, 12 GiB, 60 GiB in total, within the 7 CPU / 16 GiB / 100 GiB budget), Ubuntu 26.04 LTS, kernel 7.0.0-28-generic, rootful Docker 29.9.1 (containerd v2.4.2, runc 1.5.2), `mariadb:11.4` = `mariadb@sha256:250e1609…1873` (11.4.13), `nginx:1.27-alpine@sha256:65645c7b…2a10`, `busybox:1.37`.

## Setup (step 1)

- Shared code: the E1 setup functions (VM apply, user-v2 attach, rootful Docker, Swarm with one manager and two workers, sentinel, pinned `galera-N` services, identity markers, `--wsrep-new-cluster` removal) were moved verbatim from `e1-swarm-galera.sh` into `common.sh`. E2 calls them, so the log's setup section numbers are E1's. Two optional variables were added, and their defaults keep E1 unchanged: `GALERA_NAME` (wsrep cluster name, default `dbcm-e1`) and `SENTINEL_CONSTRAINT` (default none). E1 was not re-run. Its behavior is unchanged, and E2 exercised every moved function. `shellcheck` passes on all P0 scripts.
- E2 names: installation `e2-lab`, cluster `e2-galera`, wsrep cluster `dbcm-e2`, overlay `dbcm-e2-galera`, secret `dbcm-e2-root-pw`.
- The sentinel `sentinel-web` was pinned with `--constraint node.hostname==lima-dbcm-lab-m2`, because every disruption targets m3. An unconstrained sentinel can land on m3, as it did in E1, and would then be rescheduled on host loss. That would be expected Swarm behavior for a service with no constraint, but it would make the before/after comparison meaningless.
- Synthetic data: `e2.probe(id, note, v)` with 200 generated rows. The consistency check on each node is `SELECT COUNT(*), SUM(CRC32(CONCAT_WS('#',id,note,v)))` plus `CHECKSUM TABLE e2.probe EXTENDED`. After setup all three nodes showed `200 419677764506 / 3863630756`.
- Identity marker of galera-3 at the volume root `/var/lib/dbcm/galera-3/dbcm-identity.json`: `{"node_uuid":"559e29b2-…","installation_id":"e2-lab","cluster_id":"e2-galera","node":"galera-3"}`.

## Step 2: host loss

Command: `limactl stop --force dbcm-lab-m3` (returned at once; a hard power-off of the VM).

| Observation | Value |
| --- | --- |
| Swarm `node inspect` state of m3 `down` | 13 s after the stop. `node ls`: `lima-dbcm-lab-m3 Down Active` |
| Galera cluster size 2 on galera-1 | 13 s after the stop. galera-1/2: `Primary`, `Synced`, `wsrep_ready ON` |
| galera-3 tasks 60 s later | old task `node=lima-dbcm-lab-m3 desired=Shutdown current=Running` (stale state from the lost node); new task `desired=Running current=Pending`, error `no suitable node (scheduling constraints not satisfied on 2 nodes; 1 node not available for new tasks)` |
| Rescheduled elsewhere? | **No.** No galera-3 task ran on m1 or m2. |
| Writes on the majority | 2 rows inserted on galera-1 and 1 on galera-2, both committed. galera-1 and galera-2 consistent (`203 430054826471 / 2470712422`) |

## Step 3: returning host

Command: `limactl start --tty=false --timeout 6m dbcm-lab-m3` (exit 0 in the recorded run).

- `limactl start` returned after 14 s, and Swarm reported m3 `ready` at the same moment. m3 kept its address 192.168.104.4. The rootful Docker daemon started at boot, and so did the rootless user daemon, which Lima's docker template re-provisions on every boot. The two run side by side without conflict.
- Task restart: the pending task was scheduled on m3 and started a new container. The old task ended as `Failed … task: non-zero exit (255)`. galera-3 was `Synced` with cluster size 3 **20 s after the VM start**.
- Rejoin method: **SST, not IST.** `Found saved state: b9c3ba34-…:-1` (unclean shutdown, as after the hard kill in E1), `mariabackup SST started on joiner`, `SST completed`, then `Synchronized with group`.
- Marker verification: the marker at the volume root was byte-identical before and after (`node_uuid 559e29b2-…`). `marker-guard.sh`, run host-side in m3 with the expected identity, printed `ALLOW: marker matches node_uuid=559e29b2-… installation=e2-lab cluster=e2-galera` (exit 0).
- Data: all three nodes `203 430054826471 / 2470712422` (consistent, including the rows written while m3 was down).

## Step 4: empty-volume trap

Preparation inside m3 only: `systemctl disable docker.service docker.socket` plus `sync`, then `limactl stop --force`, then `limactl start` with rootful Docker not running (verified, and the script aborts if it is running). Then `find /var/lib/dbcm/galera-3/data -mindepth 1 -delete` and `rm -f /var/lib/dbcm/galera-3/dbcm-identity.json`. Result: an empty datadir and no marker, which models a lost or replaced disk on the old host.

Guard decision matrix (`marker-guard.sh` self-test inside m3 on synthetic temp dirs):

```
matching marker, empty datadir:               exit 0
matching marker, non-empty datadir:           exit 0
foreign node_uuid:                            exit 65
foreign installation:                         exit 65
marker missing, non-empty datadir:            exit 66
marker missing, empty datadir:                exit 67
expected identity not configured:             exit 64
```

The guard run host-side on the real wiped volume printed `REFUSE (67): marker missing and datadir empty: volume lost or replaced for existing identity node_uuid=559e29b2-…; needs an explicit manager new-identity operation`.

**4a: E1 service spec unchanged** (rootful Docker re-enabled, m3 `ready` 1 s later). Every task was `Rejected` with `invalid mount config for type "bind": bind source path does not exist: /var/lib/dbcm/galera-3/dbcm-identity.json`. Swarm created a new task about every 5 s, all on m3. This blocks the start only by accident: the marker is bind-mounted as a file. Any file at that path would pass, and a service without that mount starts.

**4b: plain Swarm service, no guard** (`docker service update --mount-rm /etc/dbcm/identity.json galera-3`): the stock service **started on the empty volume**. The entrypoint logged `Initializing database files`, the node joined via SST (`Found saved state: 00000000-…:-1, safe_to_bootstrap: 1`, `mariabackup SST completed`) and was `Synced` with cluster size 3 after **18 s**. The marker was still absent and nothing compared any identity. The new copy was consistent (`203 … / 2470712422`), but it now serves as "galera-3" with no link to the old identity. **Neither Swarm nor the image protects against this.**

**4c: guarded service.** Steps: scale galera-3 to 0, wipe the datadir again (marker still absent), then `docker service update --replicas 1 --mount-add type=bind,src=/var/lib/dbcm/galera-3,dst=/dbcm,readonly --mount-add type=bind,src=/var/lib/dbcm/guard/marker-guard.sh,dst=/dbcm-guard/marker-guard.sh,readonly --entrypoint "/dbcm-guard/marker-guard.sh docker-entrypoint.sh" --env-add DBCM_EXPECTED_NODE_UUID=<old uuid> --env-add DBCM_EXPECTED_INSTALLATION=e2-lab --env-add DBCM_EXPECTED_CLUSTER=e2-galera galera-3`. The guard wraps the image entrypoint and keeps the service's mariadbd arguments. In a 46 s window: 4 tasks `Failed … task: non-zero exit (67)`, all on m3. The service log shows `marker-guard: REFUSE (67) … database start blocked; report to manager`. The datadir stayed empty (0 entries) and the cluster stayed at size 2. Swarm keeps restarting the refused task (about every 5 s, no backoff), the same busy loop as in E1.

**4d: explicit new identity** (the manager operation in ADR 0004 item 4, done by hand here): the one-shot marker container wrote a new marker (`node_uuid ca05d274-…`), then `service update --env-add DBCM_EXPECTED_NODE_UUID=ca05d274-…`. The guard logged `ALLOW: marker matches node_uuid=ca05d274-…`. The node initialized, joined via SST and was `Synced` with size 3 after 20 s. Data consistent on all three nodes.

## Step 5: partition

The rules live inside m3 only, in a dedicated chain: `iptables -N DBCM-E2`, `-A DBCM-E2 -s/-d 192.168.104.1 -j DROP`, the same for 192.168.104.3 (m1, m2), then `-I INPUT 1 -j DBCM-E2` and `-I OUTPUT 1 -j DBCM-E2`. As a safety net, `systemd-run --unit dbcm-e2-heal --on-active=600 /bin/sh /run/dbcm-e2-heal.sh` was armed inside m3. `limactl shell` reaches the VM over vsock (`SSH_CONNECTION` is `UNKNOWN …`), so the rules do not cut off the script's own access. The script refuses to partition if the SSH source were a peer address.

- galera-3 left Primary **7 s** after the rules were applied: `wsrep_cluster_status non-Primary`, `wsrep_local_state_comment Initialized`, `wsrep_cluster_size 1`, `wsrep_ready OFF`.
- Writes and reads on the isolated galera-3 were refused: `ERROR 1047 (08S01): WSREP has not yet prepared node for application use`.
- The majority galera-1/2 reached size 2 after 7 s, stayed Primary and kept accepting writes: 2 rows on galera-1, read back on galera-2 (205 rows).
- Swarm reported m3 `Down` 13 s after the rules were applied. In the manager's view the galera-3 task went `desired=Shutdown` and a new task was `Pending` with `no suitable node`. Locally on m3 the old container kept running, because the worker could not see the manager's decision.
- Heal: the heal script ran and the timer was stopped (`remaining DBCM-E2 rules: 0`, `timer: inactive`). m3 was `ready` 2 s later. Swarm then **replaced** the galera-3 container: the old task went to `Shutdown`, a new task started on m3, and the guard allowed it (`ALLOW … ca05d274-…`). The new container found `seqno -1` and rejoined via **SST**. It was `Synced` with size 3 **29 s after the heal**.
- No row written on the isolated node exists (`isolated_rows 0`). Data consistent on all three nodes (`205 436686299911 / 3485378270`).

## Step 6: sentinel, cleanup, unrelated resources

- Sentinel after the experiment: task ID, node ID, container ID, image digest, start time `2026-10-19T14:30:38.760872295Z`, restart count 0 and response hash were **identical** to the snapshot taken before the disruptions.
- `bin/lab cleanup -apply` (run by the `EXIT` trap): stop and delete of `dbcm-lab-m1..m3`. Post-snapshot diff `removed: lima:dbcm-lab-m1, lima:dbcm-lab-m2, lima:dbcm-lab-m3`, `unrelated resources intact (3)`, exit 0. `bin/lab status` after cleanup: manifest `(empty)`. The unrelated list is unchanged before and after the run:

  ```
  unrelated (never touched):
    colima:default
    lima:thermoctl-e2e-basestation
    lima:thermoctl-test-vm
  ```

  `limactl list` after cleanup shows only the two thermoctl VMs, both still `Stopped` as before.
- The Lima `user-v2` usernet daemon of the lab exited after the VMs were deleted. Only Colima's own pre-existing usernet daemon (`~/.colima/_lima/_networks/user-v2`) is running, untouched. **`~/.lima/_networks/user-v2` still exists** (`leases.json` and two usernet log files). It is shared Lima state and was neither edited nor deleted.
- Host free disk: 75 GiB before apply, 74 GiB after cleanup. The 1 GiB difference was not investigated. Lima's image cache and unrelated host activity are both possible causes, and nothing named `dbcm-lab-*` remains.

## Failures and surprises (all observed)

1. **Writes inside a VM are lost on `limactl stop --force` unless synced.** In development run 1, the guard script installed in step 3 (about 6 s before the forced stop in step 4) and a `systemctl disable docker.service docker.socket` were both gone after the restart. As a result, rootful Docker started at boot and the guard file did not exist (`sh: cannot open …/marker-guard.sh`, exit 2). The run was aborted. The script now runs `sync` after installing the guard and after the disable, verifies that rootful Docker is not running before it wipes the volume, and checks that the guard file survived. For the product: anything the agent writes before a possible power loss, such as the identity marker, must be fsynced.
2. **Every return after an unclean stop used SST, not IST**: the returning host (step 3) and the partition heal (step 5, where Swarm replaced the container). The node reports `seqno -1` in `grastate.dat`, as after the hard kill in E1. A `--wsrep-recover` pre-start step is still unevaluated.
3. **Swarm replaces the task after a partition longer than node-down detection** (about 13 s here). The manager sets the old task to Shutdown and starts a new one on m3 once the node reconnects, so a healed partition causes a container restart and an SST. Task timestamps after the heal read `new task Running 37 s ago` and `old task Shutdown 34 s ago`. The order in which the old container actually stopped and the new one started was not observed directly; the timestamps are the manager's status reports. A dedicated check (for example `docker events` on m3) is needed to rule out a short overlap of two containers on the same datadir. The new mariadbd did not report an InnoDB lock error.
4. The E1 marker **file** bind mount made Swarm reject the task when the marker was missing (4a). This looks like protection but is incidental (see 4a).
5. A refused start loops forever (exit 67 about every 5 s). The guard has to be combined with a restart policy limit, or the agent has to stop the service and report.
6. `limactl start` exited 0 in all three runs, although Lima's docker template re-runs its rootless provisioning at boot. The script tolerates and records a non-zero exit while the VM is running, but that path was not exercised.

## ADR 0004 assumptions

| Assumption | Result |
| --- | --- |
| Pinned service is not moved by the scheduler | **Confirmed for host down and network partition.** The replacement task stayed `Pending` with `no suitable node`, and no galera-3 task ran on m1/m2 in any step. |
| Swarm restarts the node on the same host when it returns | Confirmed (step 3: new task on m3, Synced after 20 s). After a partition, Swarm also **replaces** the still-running container (surprise 3). |
| Galera quorum on host loss and partition | Confirmed: the 2-node majority stays Primary and writable. The isolated node goes non-Primary and refuses reads and writes (ERROR 1047). After healing no divergent data remains. |
| Marker on the node's local volume survives host loss | Confirmed at the volume root (unchanged after a hard power-off and SST). It must be fsynced when written (surprise 1). |
| Guard against an empty volume | **Swarm and the stock image do not provide it**: the plain service started on the empty volume and joined as galera-3 (4b). A pre-start guard as the entrypoint wrapper blocked the start (exit 67, datadir untouched, 4c) and allowed it after an explicit new-identity step (4d). In the product, the agent runs the check and the manager supplies the expected identity; the lab passes it as service env. |
| Returning host is reconciled by its marker | The guard can tell the old identity (ALLOW), a foreign one (65), data without a marker (66) and a lost volume (67). Retire versus rejoin as a manager decision was not exercised. In the lab, Swarm rejoined automatically because the marker matched. |

## Open items

- Exact container stop/start ordering on partition heal (surprise 3).
- `--wsrep-recover` pre-start to allow IST after unclean shutdowns.
- Restart backoff/max-attempts for refused or never-joining nodes.
- Graceful host reboot and `docker node update --availability drain` remain untested (only power-off and partition were tested).
- Three-manager Swarm and loss of the manager host remain untested. In E2 only a worker was disrupted.
- `~/.lima/_networks/user-v2` is shared state that is not tracked by the ownership manifest (as in E1).
