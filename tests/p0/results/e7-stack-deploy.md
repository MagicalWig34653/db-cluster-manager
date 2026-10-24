# E7: `deploy/stack.yaml` with a `manager init` configuration on lab VMs: all checks passed, exit 0

Date: 2026-10-24, log UTC 08:34Z-08:40:27Z. Branch `w05-stack-deploy`, product code at `integration` (no product code changed). Raw log: [`e7-stack-deploy.log`](e7-stack-deploy.log). Script: [`tests/p0/e7-stack-deploy.sh`](../e7-stack-deploy.sh). Earlier runs: attempt 1 failed in the script (`cd` into a root-owned directory without root, nothing deployed; log not kept); attempt 2 passed with `E7_KEEP_VMS=1` ([`e7-stack-deploy-dev.log`](e7-stack-deploy-dev.log)) and showed one script bug (the monitoring check read a password file the script had not created), fixed for attempt 3 (this record).

## Topology

Three lab VMs (`bin/lab apply -vms 3 -cpus 2 -ram 4 -disk 20 -network lima:user-v2`, arm64, rootful Docker 29.9.2): m1 the only Swarm manager node and the pinned manager task, m2/m3 workers. That is 12 GiB RAM and 6 vCPU, within the lab budget (16 GiB, 7 vCPU). The image was built **inside m1** with `docker build -f deploy/Dockerfile` from `git archive HEAD` (base images pulled from their registries, pinned by digest; the Node/UI stage and the Go stage both ran: 35 s in attempt 3, about 69 s in attempt 2), copied to m2/m3 with `docker save | docker load`; nothing was pushed. Host load was 47-375 during the runs (foreign load).

## Results

| Check | Result | Evidence |
|---|---|---|
| Image from `deploy/Dockerfile` | passed | arm64, `user=65532:65532`, entrypoint `/usr/local/bin/manager`, 48 MB; `manager -version` works |
| `manager init` from the image | passed | produces the configuration (same shape as `deploy/manager.example.json`) and the instructions; stored as Swarm config `dbcm-manager-config-1` |
| `docker stack deploy -c deploy/stack.yaml` | passed | task Running within 6 s. Swarm stored constraints `node.role == manager`, `node.hostname == lima-dbcm-lab-m1`, replicas 1, `user=65532:<docker gid>`, read-only root, `cap_drop ALL`, stop-first/rollback. The container got read-only rootfs, no capabilities, `netmode=host`, health `healthy`, volume `dbcm_manager-data` writable by 65532 (`data.key`, `metastore/`, `monitoring/` created) and the Docker socket. This resolves the "untested stack options" of ADR 0014 for engine 29.9.2 |
| Manager reachable | passed | `/api/v1/health` 200, `/api/v1/version` `0.0.0-e7` `linux/arm64`; listeners only `127.0.0.1:8080`, `127.0.0.1:7445`, `<m1 address>:7444` |
| UI served | passed | `GET /` 200 HTML, the referenced `/_app/...js` asset 200 `text/javascript` |
| Authenticated | passed | unauthenticated `GET /api/v1/clusters` 401; setup token read from `docker service logs`; first administrator through the WebAuthn flow (software passkey); authenticated request 200 |
| Agents connect with mTLS | passed | Swarm connected, three hosts enrolled (`enroll: agent enrolled host=...` x3, `agentIdentity: true`); agent port 7443 refuses a client without a certificate (curl exit 56); plain HTTP to the enrollment port 7444 gets 400 |
| Galera cluster create through the API | passed | 3-node `e7db`, operation succeeded in about 100 s; MariaDB checks (status, replicated writes on n1/n3, reads on all nodes, accounts, monitoring account over the socket) |
| Manager container `docker kill` | passed | new task Running in 6 s, leading again 8 s after the kill (term 2 to 3); the old session cookie still valid; cluster record and `healthy` view intact; `wsrep_cluster_size` 3 |
| `docker service update --force` | passed | stop-first: old task shut down, new task up, leading again 14 s later, session and cluster intact |
| Drain of the pinned manager node m1 | behaves as documented, with a visible outage | the manager task goes `Pending` ("no suitable node (scheduling constraints not satisfied ...)") because the constraint allows no other node; the API is down (connection refused) while drained. The drain also moved the agent task on m1 and took the Galera slot n3 (on m1, local volume) to `Pending`: the cluster ran on 2 of 3 nodes. After `node update --availability active` the manager task started within 9 s, leading 54 s after the drain began; session, setup state and cluster record were intact (volume retained); n3 and the agents returned and `wsrep_cluster_size` was 3 again |
| Peer-TLS refusal with a wrong certificate | **not possible from the stack** | `deploy/stack.yaml` deploys exactly one replica by design (ADR 0014, item 4). The evidence for a replica with a wrong certificate remains the plain-process run [e4-rerun-B2](e4-rerun-B2.md). The per-replica Swarm-service layout in `docs/INSTALL.md` ("Three-replica variant") was **not** run |
| Cleanup | passed | `bin/lab cleanup -apply` removed `lima:dbcm-lab-m1..m3`; `unrelated resources intact (3)` (`colima:default`, `lima:thermoctl-e2e-basestation`, `lima:thermoctl-test-vm`) |

## Findings and limits

- No product bug was found; no product code changed. The script needed two fixes (root for `cd`/`docker build`/`docker stack deploy` in the root-owned source directory; the lab monitoring password file).
- `docker service logs` keeps the lines of earlier tasks, so the manager log shows the history across restarts.
- Before the drain, `docker service ls` showed the global agent service as `1/3` although all three hosts reported `agentIdentity: true` (observed in the attempt-2 log; not investigated).
- A killed manager container leaves a failed task (`non-zero exit (137)`) in the task history; Swarm schedules the replacement itself.
- Not covered: a TLS reverse proxy in front of the UI (the lab used `http://localhost` as the passkey origin, with the client inside m1), backups from the stack (the `backend` secret option), failure of the Swarm manager node itself, registry pulls (images were copied), amd64. Timings are single samples under foreign host load.
- Host free disk after cleanup: 21 GiB of 926 GiB; reclaimed space was not measured by a controlled method.
