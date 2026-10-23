# E5 and E6: database proxy against real Galera, full-cluster outage and guided recovery

Date: 2026-10-23. Script: [`tests/p0/e5-e6-proxy-outage.sh`](../e5-e6-proxy-outage.sh) (sources the E4 harness [`tests/p0/e4-manager-galera.sh`](../e4-manager-galera.sh) as a library; phases in [`e5-phases.sh`](../e5-phases.sh) and [`e6-phases.sh`](../e6-phases.sh)). Lab SQL client: [`tests/p0/e5client`](../e5client/main.go) (go-sql-driver/mysql, the driver of `internal/galeraprobe`). Logs (sanitized):

- [`e5-e6-proxy-outage.log`](e5-e6-proxy-outage.log): the **final run from scratch**, every phase, exit 0 (03:50Z-04:06Z, code at commit `dc57907` plus the test-harness changes committed with this file). VMs created and removed by the run.
- [`e5-e6-proxy-outage-dev.log`](e5-e6-proxy-outage-dev.log): the development run on kept VMs (03:07Z-03:47Z), including the failures below and the in-place upgrades of manager and agents after each fix. Its first E6 phases ran with code without the recovery operation; a manual `mariadbd --wsrep-recover` on n1 (to find the command line) is not in that log and is noted under F-E6-3.

## What ran

The E4 environment, unchanged: three arm64 Lima VMs from `bin/lab apply -apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2` (6 vCPUs, 12 GiB, 60 GiB of the 7/16/100 budget), Ubuntu 26.04, Docker 29.9.2 rootful, Swarm m1 manager + m2/m3 workers, the real `bin/manager` with auth and mTLS agents, a 3-node MariaDB 11.4.13 Galera cluster created through `POST /clusters` (about 100 s). Host: 14 logical CPUs, 48 GiB; foreign 1-minute load 5-10 during the final run (43-74 for a while in the development run), so the final timings are less disturbed than E4's. Host free disk 62 GiB before, 60 GiB with VMs, 62 GiB after cleanup (df, 1 GiB resolution).

E5 layout: `cmd/proxy` (host cross-compiled) runs as a plain container on m1 attached to the cluster's attachable overlay network and reaches the nodes by their dnsrr service names. Two instances: `dbcm-e5-proxy` (plaintext, `single-writer-stable`, read endpoint `exclude-writer`) and `dbcm-e5-proxy-tls` (`prefer-node n2`, TLS termination with a lab CA certificate). Both: `liveness mysql-greeting`, `sync galera` through the monitoring account `dbcm_e5mon@'%'` (USAGE only, TLS `disabled` because the nodes have no product TLS), probe interval/timeout 1 s, `stale_after 5s`, `drain_deadline 10s`. Clients run inside the proxy containers.

E6: two full-cluster outages caused by the operator outside the manager (the manager has no "stop cluster" command): **clean** (`docker service scale` of all three services to 0 in one command) and **crash** (`docker kill --signal KILL` of all three containers in parallel, then scale to 0 within the Swarm restart delay so nothing restarts; seqno -1 everywhere, as after a power loss).

## E5 results (final run)

| Check | Observed |
| --- | --- |
| Writer selection | Stable proxy: first decision after 1 s chose `n3` ("lowest latency"; n3 runs on m1 next to the proxy: direct p50 15 µs vs 118-121 µs to n1/n2). Only n3 had reported at that moment ("n1: unhealthy n2: unhealthy"). TLS proxy: also n3 first, then `n3 -> n2 after 3 consecutive evaluations` (prefer-node) 2.3 s later. |
| Read endpoint | 99 writes on 99 new connections: n1 50, n2 49, never the writer. |
| Overhead | `SELECT 1` through the write endpoint p50 30 µs vs 15 µs direct to the same node; connect+auth 0.63 ms vs 0.48 ms. TLS endpoint: connect+auth 2.1 ms, p50 142 µs (writer n2 on another VM). |
| Writer killed under load | `docker kill` of n3 at 03:55:25.2; the proxy switched to n2 at 03:55:25.4. Both clients (one persistent connection; one new connection per write, 10 writes/s) failed from 03:55:25.3 and succeeded again at 03:55:31.4: **6.2 s without a successful write**, which matches Galera's own failure detection of the dead member, not the proxy (the proxy had already switched). Errors seen: `invalid connection`, `Error 1105: dbcm-proxy: backend n3 unreachable` (connections that were routed in the 0.2 s before the switch). |
| Second writer change (donor) | When Swarm had restarted n3, n3 rejoined with SST from n2; n2 went `donor`, so at 03:55:36.4 the proxy moved the writer again (`n2 -> n1`) and closed the persistent connection to n2 at the 10 s drain deadline (client error at 03:55:46.4, reconnected in 0.1 s). One node failure therefore caused **two writer changes**. In the development run a write on the donor writer hit a 3 s client read timeout, so moving away from a donor is justified, but it is a second interruption for long-lived connections. |
| Hysteresis | After n3 was healthy/synced again the stable proxy kept n1 (no flap back; `writer_changes 3` incl. the initial choice). The prefer-node proxy moved back to n2 only after 3 consecutive evaluations (03:55:50.8). |
| Planned switch with drain | SIGHUP with `prefer-node n3`: switch after 3 evaluations (2 s); the persistent connection kept working on the old writer until the 10 s deadline, then one error, reconnected after 0.2 s on n3 (longest gap 0.4 s). |
| TLS termination | go-sql-driver with the lab CA: works (`Ssl_cipher` on the node session is empty: the proxy-to-node link is plaintext, documented limit). Without TLS: refused with `ERROR 1043 ... TLS is required for this endpoint`. Wrong server name: certificate verification error. MariaDB 11.4 CLI with `--ssl-ca --ssl-verify-server-cert`: `Cipher in use is TLS_AES_128_GCM_SHA256, cert is OK`; with `--skip-ssl`: refused (1043). |
| Plaintext L4 path | The 11.4 CLI through the plaintext proxy negotiates TLS end to end with the node's auto-generated self-signed certificate (`cert is UNKNOWN`); with the lab CA and verification it fails (`self-signed certificate`), as expected: nodes have no product-issued certificate. |
| Probe side effects | `Aborted_connects_preauth` 402-518 per node after about 4 minutes of two proxies; 521 `Aborted connection` lines in n2's log. `skip_name_resolve=1`, `max_connect_errors=100`, `performance_schema=0`: no host was blocked. The value comes from the image's configuration file, so the product now sets it explicitly (F-E5-2). The log noise remains. |

## E6 results (final run)

| Check | Observed |
| --- | --- |
| Clean outage, ground truth | All three `grastate.dat`: same UUID, `seqno: 3338`, **`safe_to_bootstrap: 0` on all nodes** (the simultaneous scale-down left no "last node"). |
| Clean diagnosis (`GET .../outage-diagnosis`) | `candidate n1`, all positions 3338 from grastate, risk `tie` ("lowest slot chosen"), `requires_approval: true`. Correct: equal seqno means the same committed state, any node is safe. Same answer on a second call. |
| Hands off | 120 s observation: no service spec version changed, no task ran, no new operation; cluster state `unknown` (clean) / `unavailable` (crash). No event was recorded for the full outage. |
| Guided recovery (clean) | `POST .../outage-recovery {"candidate":"n2"}` (differs from the diagnosis): `409 conflict`. With `n1`: `202` with digest; 20 s later the operation still waited and all specs had 0 replicas; wrong digest: `409 approval_mismatch`; approval: `succeeded` after 40 s. Start order from the task timestamps: n2 04:02:17, n3 04:02:24, n1 04:02:30 (the bootstrap task of n1 started before n2 and was replaced by the steady-state spec at the end, as planned). All nodes Synced, size 3, **same cluster state UUID**; marker row present; a new write worked. |
| Crash outage, ground truth | All `seqno: -1`, `safe_to_bootstrap: 0`, `gvwstate.dat` present. Swarm had already created replacement tasks (state `Ready`/`Assigned`) when the scale-down arrived; none ran. |
| Crash diagnosis (`GET`) | No candidate: "positions known for 0 of 3 nodes, need 2"; risks `unclean-shutdown` and `position-unknown: run mariadbd --wsrep-recover on this node` for every node. `POST .../outage-recovery` would be refused (no candidate, unit-tested; the final run asks only after the position recovery). |
| Position recovery (`POST .../outage-diagnosis`) | The agents ran `mariadbd --wsrep-recover` in one-off containers (about 1 s for this small dataset): all three `seqno 3345` (source `wsrep-recover`), candidate n1, risks `unclean-shutdown` and `tie`. grastate.dat unchanged, no spec changed, no one-off container left. Same answer on a second call. |
| Guided recovery (crash) | Wrong candidate: 409; wrong digest: 409; approval: `succeeded` after 61 s. mark-safe wrote `seqno 3345` and `safe_to_bootstrap: 1` into n1's grastate.dat; joiners started from `-1` with `restore pc from disk successfully` and SST. **Cluster state UUID kept**; rows 100-102 present. |

Joiners received a full SST in both recoveries, also at an equal seqno, and the candidate took an SST after its final restart (as in E1/E4). With large datasets this dominates the recovery time; why IST was not used was not investigated.

## Failures found and fixed

| # | Observed | Root cause | Fix |
| --- | --- | --- | --- |
| F-E6-1 | No way to execute the guided recovery: the diagnosis plan was advice only (`runner.FromGaleraPlan` refused it), so a full outage could only be recovered by hand. | Not implemented (A06/R1 scope). | `POST /clusters/{id}/outage-recovery` (operate permission): fresh diagnosis, refuses a candidate other than the diagnosed one, stores the plan as an operation whose first stop waits for the digest-bound approval; plan steps stop-db, await-stopped, verify-identity, mark-safe-to-bootstrap, bootstrap spec with 0 replicas, start-service, wait-synced, joiners one at a time, steady spec, await-service. New agent commands `await-stopped`, `start-service`, `mark-safe-to-bootstrap` (`e7eb785`, `07b529e`). |
| F-E6-2 | Development run: after the crash the diagnosis correctly proposed nothing, and the product could not obtain the positions its own guidance asks for. | `--wsrep-recover` was not implemented anywhere. | Agent command `wsrep-recover` (one-off container without network, database user, unreachable cluster address, refused while a container mounts the volume) and `POST /clusters/{id}/outage-diagnosis` (operate permission, refused while a node was last seen in a Primary component). `GET` stays read-only (`e7eb785`, `07b529e`). |
| F-E6-3 | First manual try of the command line: `mariadbd --wsrep-recover` aborted with `rsync SST method requires wsrep_cluster_address`, then the same for mariabackup. | mariadbd needs a cluster address even for `--wsrep-recover`. | The agent passes `--wsrep_cluster_address=gcomm://127.0.0.1` (unreachable without network; never the bootstrap address `gcomm://`); then `WSREP: Recovered position: <uuid>:3525`. |
| F-E6-4 | Development run, crash recovery through the product: operation `succeeded`, data intact, but the **cluster state UUID changed** (`3b2d2499...` to `618d2058...`, seqno restarted at 0); every joiner took a full SST from the new history. | mark-safe set only `safe_to_bootstrap: 1`; the candidate bootstrapped from `seqno: -1` without a start position and Galera started a new history. | mark-safe also writes the recovered seqno when grastate says -1 (`MarkSafeArgs.SetSeqno`, plan field `Step.Recovered`); final run: UUID kept (`07b529e`). |
| F-E5-1 | Read endpoint looked like it always used one node. | Test client defect: INSERT and SELECT used two connections, so round robin over two readers always put the SELECT on the same node. | e5client uses one connection per write. Not a product defect. |
| F-E5-2 | Probes abort about two handshakes per second and node. | No defect yet: the image sets `skip-name-resolve`; another image would let `max_connect_errors` block the proxy host. | `--skip_name_resolve=ON` in the Galera service args (`dc57907`). |

Script defects fixed on the way: the running-task count filtered on desired state and missed tasks being removed (the first clean "ground truth" was read while nodes were still shutting down, development log); slot hosts were resolved from running tasks and were empty while all services were at 0; the TLS CLI check first addressed the other proxy container by name.

## What remains broken or untested

- Full outage produces **no event or notification**, and the cluster state is `unknown`/`unavailable` rather than a distinct outage state. The operations API shows `running` with no reason while the recovery waits for approval, and no step progress.
- Approving a recovery uses the restore permission of `POST /operations/{id}/approve` (a dedicated recovery permission is not modelled).
- `no-node-running` is checked once by the await-stopped steps; something that scales a service up afterwards (outside the manager) is only caught by mark-safe's position check, not by a running-task re-check before the bootstrap.
- Joiners start from seqno -1 and take SST; the recovered seqno is written only for the candidate. SST was also used at equal seqno after the clean outage.
- Not run: a power loss of the VMs (with Docker and the manager restarting), diverging histories, a node missing during the diagnosis, a recovery interrupted by a manager failover, large datasets (wsrep-recover and SST time), 5-node clusters.
- Proxy: one instance per endpoint only; no redundant proxies, VIP, DNS failover or proxy loss; no product deployment of the proxy (lab container); no node TLS and no proxy-to-node TLS; the monitoring account for a proxy host is an operator step (the product account is `@localhost`); authentication plugins other than `mysql_native_password`, other connectors, transactions across a switch; probe log noise (one `Aborted connection` line per probe).
- amd64 (all runs arm64 Lima VMs).

## Cleanup and unrelated resources

Development VMs: `bin/lab cleanup -apply` after the development run (`removed: lima:dbcm-lab-m1..m3`, `unrelated resources intact (3)`). Final run: EXIT trap `bin/lab cleanup -apply`; afterwards the manifest was empty and `colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm` (both `Stopped` before and after) were reported intact. No other process or profile was touched. Host-side binaries lived in the git-ignored `.lab/e4/`.
