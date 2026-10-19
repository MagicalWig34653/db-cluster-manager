# E1: Swarm on three lab VMs, sentinel, pinned Galera services

Date: 2026-10-19. Script: [`tests/p0/e1-swarm-galera.sh`](../e1-swarm-galera.sh) (helpers in [`tests/p0/common.sh`](../common.sh)). Full sanitized log of the recorded run: [`e1-swarm-galera.log`](e1-swarm-galera.log). The figures below come from that run, a complete run from scratch (`tests/p0/e1-swarm-galera.sh`, no environment overrides, exit 0, 13:58:06Z to 14:02:59Z, about 5 minutes including VM creation and cleanup). Earlier development runs on kept VMs (`E1_REUSE_VMS=1 E1_KEEP_VMS=1`), two of which failed, and one failed full run are described under "Failures and surprises". Their logs were overwritten; only the final run's log is kept.

All results are native arm64 (Apple Silicon host, `vz` VMs, arm64 images). AMD64 was not tested.

## Environment

| Item | Value |
| --- | --- |
| Host | 14 logical CPUs, 48 GiB RAM (from `bin/lab status`); lab budget 7 CPUs / 16 GiB / 100 GiB |
| Lima | `limactl version 2.2.0`, VM type `vz`, `template:docker` |
| Lab VMs | `dbcm-lab-m1..m3`, each 2 vCPUs, 4 GiB RAM, 20 GiB disk (total 6 CPUs, 12 GiB, 60 GiB, within budget) |
| Guest | Ubuntu 26.04 LTS, kernel 7.0.0-28-generic, aarch64, `nproc` 2, 3896 MiB total memory, about 3.4 GiB available after boot |
| Docker (guest, rootful) | Engine 29.9.1, containerd v2.4.2, runc 1.5.2; cgroup driver systemd, storage overlayfs, AppArmor + seccomp |
| MariaDB | `mariadb:11.4` = `mariadb@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873`, arm64, version 11.4.13, Galera 4 (`/usr/lib/galera/libgalera_smm.so`) |
| Sentinel | `nginx:1.27-alpine` = `nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10`, arm64 |
| One-shot marker image | `busybox:1.37` = `busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e`, arm64 |
| Host free disk (data volume) | 76 GiB before apply, 74 GiB with the VMs running and images pulled, 76 GiB after cleanup |

## Steps and observations

### Step 1: VMs

`bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20` created and started the three VMs (about 2 minutes). Post-snapshot diff: only `lima:dbcm-lab-m1..m3` added.

### Step 1b: guest preparation (deviation from the plan)

Two changes inside the lab VMs were needed before Swarm could work. Both affect only the lab VMs.

1. **Network.** With Lima's default user-mode network every VM has `eth0 192.168.5.15/24`, and the VMs cannot reach each other. The Lima docs say: "This IP address is not accessible from the host by design. Use VMNet to allow accessing the guest IP from the host and other guests." The docs also describe `user-v2`: "user-v2 network provides a user-mode networking similar to the default user-mode network and also provides support for `vm -> vm` communication." The script stops each lab VM, runs `limactl edit --tty=false --network=lima:user-v2 dbcm-lab-mN`, and starts it again. The `user-v2` network was already defined in Lima's default `networks.yaml`, so that file was not edited. It needs no sudo and no socket_vmnet, and it makes no host network change. Lima starts a user-level `limactl usernet` daemon for it (state under `~/.lima/_networks/user-v2`). The unrelated VMs do not use `user-v2`: `thermoctl-e2e-basestation` uses `vzNAT`, and Colima runs its own separate usernet daemon under `~/.colima/_lima/_networks`, which was left untouched. After the change the VMs got 192.168.104.1, .3 and .4. `lima-dbcm-lab-mN.internal` resolved from every VM, and ping and TCP between the VMs worked (rtt about 0.5 ms). Surprise: m1 received 192.168.104.1, the address that `networks.yaml` lists as the `user-v2` gateway. The real default route is 192.168.104.2. This did no harm here.
2. **Rootful Docker.** `template:docker` runs **rootless** Docker, and the system `docker.service` and `containerd.service` are masked. A rootless `docker swarm init --advertise-addr 192.168.104.1` succeeded on m1 during development. But the daemon runs inside the rootlesskit network namespace (`--net=gvisor-tap-vsock`), so the Swarm ports (2377, 7946, 4789) are not on the VM's interface, and Docker documents overlay networks as unsupported in rootless mode. So the rootless user service was disabled, and `containerd.service`, `docker.socket` and `docker.service` were unmasked and enabled. Every docker command then ran as `sudo docker` inside the VM. Enabling `docker.service` alone failed: dockerd timed out on `/run/containerd/containerd.sock` until containerd was unmasked too.

### Step 2: Swarm

`docker swarm init --advertise-addr 192.168.104.1` on m1. m2 and m3 joined as **workers** with their own `--advertise-addr`, so the Swarm has **one manager only**. That is acceptable for E1 but provides no manager HA. The join token was read with `swarm join-token -q` into a variable and never logged. `docker node ls`: three nodes `Ready/Active`, m1 `Leader`, Engine 29.9.1.

### Step 3: sentinel

`sentinel-web` (nginx, replicas 1, published `18080:80` via the routing mesh, label `dbcm.test=sentinel`, no placement constraint) answered 4 s after creation. Swarm placed it on m3. Snapshot: task `pry4g6bwjfuexp671uxjs6va2`, node `z4zp15cc49dfljfv2rx9q1kpf`, container `276155dfe1ce…`, image `nginx:1.27-alpine@sha256:65645c7b…`, started `2026-10-19T14:01:13.939785661Z`, restarts 0, response body sha256 `fb47468a…e8d6de` (fetched via the mesh from m1).

### Step 4: pinned Galera services

- Attachable overlay network `dbcm-e1-galera` and Docker secret `dbcm-e1-root-pw` (18 random bytes generated inside m1, never printed). Both carry `dbcm.installation` labels.
- Identity marker: on each host, a one-shot `busybox` container (`docker run --rm`, labelled) created `/var/lib/dbcm/galera-N/data` (owner 999) and wrote `{"node_uuid":…,"installation_id":"e1-lab","cluster_id":"e1-galera","node":"galera-N"}` to `/var/lib/dbcm/galera-N/dbcm-identity.json` at the volume root. It also wrote a copy inside the datadir, to test whether a marker there survives. The bind source has to exist before the service is created, so the one-shot step is required.
- Services `galera-1..3`: `--replicas 1 --constraint node.id==<id of dbcm-lab-mN> --network dbcm-e1-galera --endpoint-mode dnsrr --hostname galera-N --restart-condition any --stop-grace-period 60s`, labels `dbcm.installation/cluster/node`, bind mounts `/var/lib/dbcm/galera-N/data → /var/lib/mysql` and the marker read-only at `/etc/dbcm/identity.json`, root password via `MARIADB_ROOT_PASSWORD_FILE=/run/secrets/dbcm-e1-root-pw`, and `MARIADB_MYSQL_LOCALHOST_USER=1` with `MARIADB_MYSQL_LOCALHOST_GRANTS=RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR` for mariabackup SST.
- Galera options (mariadbd arguments):
  - galera-1 at first: `--wsrep_on=ON --wsrep_provider=/usr/lib/galera/libgalera_smm.so --wsrep_cluster_name=dbcm-e1 --wsrep_cluster_address=gcomm://galera-1,galera-2,galera-3 --wsrep_sst_method=mariabackup --binlog_format=ROW --innodb_autoinc_lock_mode=2 --wsrep_provider_options=gcache.size=128M --wsrep-new-cluster`
  - galera-2/3, and galera-1 after formation: the same without `--wsrep-new-cluster`.
- galera-1 bootstrapped (Synced, size 1) in 5 s. The cluster address names services that did not exist yet (with dnsrr their names do not resolve), and that did not prevent the bootstrap. galera-2 joined via **SST** (mariabackup) in 18 s, then galera-3 in 18 s. Their logs show `mariabackup SST completed on joiner`, followed by a short IST for the write sets queued during the SST.
- galera-1 was then updated with `docker service update --args "<options without --wsrep-new-cluster>"`, so a later restart cannot bootstrap a second cluster. Swarm replaced the task on the same node (m1). The node rejoined via **IST** (`Receiving IST: 2 writesets, seqnos 4-5`) and was Synced at size 3 after 9 s.
- `docker service ps`: each `galera-N` task ran on `lima-dbcm-lab-mN`, as pinned.

### Step 5: cluster state and replication

All three nodes reported `wsrep_cluster_size=3`, `wsrep_cluster_status=Primary`, `wsrep_local_state_comment=Synced`, `wsrep_ready=ON`, `wsrep_connected=ON`, the same `wsrep_cluster_state_uuid` and the same `wsrep_last_committed`. `wsrep_incoming_addresses` listed overlay addresses 10.0.1.x. A row `synthetic-row-from-galera-1` was inserted into `e1.probe` on galera-1 and read back on galera-2 and galera-3.

### Step 6: `docker kill` of galera-2

- `docker kill` on m2 → the task failed with `task: non-zero exit (137)`. Swarm started a **new task on the same node** (`lima-dbcm-lab-m2`; the task history shows the failed and the new task, both on m2). The new container was running **5 s** after the kill, with the default restart delay.
- A write on galera-1 started right after the kill took **5 s**: it stalled until Galera removed the dead member from the view, then committed. galera-1 then reported cluster size 2.
- The restarted galera-2 found `grastate.dat` with `seqno: -1` (unclean shutdown; `Local state: 00000000-…:-1`). It requested a **full SST, not IST** (`IST uuid:00000000-… f: 0, l: 11`, `mariabackup SST completed on joiner`). It was `Synchronized with group` and the cluster was back at **size 3, Primary, 19 s after the kill**. The row written while it was down was present on galera-2.
- So a hard kill leads to SST, not IST, with this stock image configuration: the image's entrypoint and mariadbd do not run `--wsrep-recover`, so the node does not recover its seqno from InnoDB before joining. A graceful stop (the galera-1 args update in step 4) used IST.

### Step 7: sentinel and cleanup

- Sentinel after the experiment: task ID, node ID, container ID, image digest, start time, restart count (0) and response hash were **identical** to step 3.
- `bin/lab cleanup -apply` (run from the script's `EXIT` trap) stopped and deleted the three VMs. Post-snapshot diff: only the three lab VMs removed, `unrelated resources intact (3)`. `bin/lab status` after cleanup: manifest empty; unrelated `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`, as before. Both thermoctl VMs were still `Stopped`, as they were before the run.
- The lab's `user-v2` usernet daemon exited after the last lab VM was deleted. Only Colima's own pre-existing usernet daemon (started 2026-10-18) is running, untouched. The directory `~/.lima/_networks/user-v2` (lease file and two small log files) remains. It is not tracked by `bin/lab` and was not removed.
- Host free disk after cleanup: 76 GiB (same as before).

## Failures and surprises (all observed)

1. Default Lima networking has no VM-to-VM path (all VMs at 192.168.5.15); `lima:user-v2` was required (step 1b).
2. `template:docker` is rootless Docker; Swarm and overlay networking need the rootful daemon, and enabling it requires unmasking containerd too (step 1b).
3. `--innodb_buffer_pool_size=256M` on the mariadbd command line breaks mariabackup SST: `wsrep_sst_mariabackup` passes the server options to `mariadb-backup`, which aborts with `unknown variable 'innodb-buffer-pool-size=256M'`. The joiner then aborts too (exit 133), and Swarm restarted it in an endless loop (a new task about every 19 s, with no backoff limit). The option was removed.
4. Without `MARIADB_MYSQL_LOCALHOST_USER=1`, SST fails on the donor with `Access denied for user 'mysql'@'localhost'`: the image does not create the socket-authenticated `mysql` user by default. Same endless restart loop as in 3.
5. The marker copy **inside the datadir was deleted by SST** on galera-2 and galera-3 (mariabackup SST cleans the datadir). It survived only on galera-1, which never received an SST. The marker at the volume root (outside `/var/lib/mysql`) survived on all three nodes, through SST and through the kill/restart.
6. A hard kill caused a full SST instead of IST (step 6).
7. One full run failed in step 4 because the `nginx` image was not present on m1 when its digest was recorded (Swarm had placed the sentinel on another node). The `EXIT` trap ran the scoped cleanup correctly on that failure path. The script now pulls the image on every VM, only for the digest record.

## ADR 0004 assumptions

| Assumption | Result |
| --- | --- |
| One service per DB node, replicas=1, `node.id` constraint → Swarm restarts on the same host and does not move it | **Confirmed for task failure** (`docker kill`) and service update: the replacement task stayed on the pinned node. Host down, drain and reboot are **untested** (E2). |
| Restart policy restarts the container | Confirmed (`--restart-condition any`, 5 s). But Swarm also restarts a node that can never join, endlessly (surprises 3 and 4). A max-attempts/backoff policy and manager-side failure detection are needed. |
| Marker file on the node's local volume | **Refined:** the marker must live **outside** the MariaDB datadir. Inside it, SST deletes it. Layout that worked: volume root `/var/lib/dbcm/galera-N/` with the marker plus `data/` as the datadir. |
| Bind mount vs named volume | Bind mounts worked. The source directory must exist before the service is created (one-shot step). Named volumes untested. |
| Guard against empty volume / foreign marker | **Not provided by Swarm or the image.** Nothing checked the marker before start; the read-only marker mount is not a guard. The pre-start guard (agent) is still to be demonstrated (E2 step 4). |
| Bootstrap is a manager operation | `--wsrep-new-cluster` must be removed after formation; if it stays in the service spec, any restart of galera-1 would bootstrap a new cluster. The manager therefore has to change the service spec after bootstrap. |
| Labels for ownership bookkeeping | Labels were set on services, network, secret and one-shot containers; inventory by object ID not exercised. |
| ADR premise "Swarm on Docker hosts" | The lab needs rootful Docker. Rootless Docker (as shipped by Lima's docker template) does not support Swarm's overlay networking in a usable way. Supported Docker modes are a product/requirements question for real hosts. |

## Open items

- `bin/lab` cannot request a network: `limactl edit --network=lima:user-v2` plus a VM restart is done in the script. A `-network` option in `bin/lab` (recorded in the manifest) would make this part of the inventory. `~/.lima/_networks/user-v2` is not tracked by the ownership manifest.
- Only one Swarm manager. Three-manager Swarm and manager loss are untested.
- `wsrep_node_address` was not set; Galera picked the overlay interface automatically. Whether this holds with several networks attached is untested.
- No restart backoff/max-attempts was configured; behavior when Galera repeatedly fails is a busy loop.
- SST after a hard kill: evaluate a `--wsrep-recover` pre-start step (agent) so that IST can be used.
- Host loss, returning host with the old volume, empty-volume trap and partition: E2.
- The `WAIT_ELAPSED_FILE` helper in `common.sh` leaves one empty temp file per run in `$TMPDIR` (cosmetic).
