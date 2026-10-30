---
title: Troubleshooting
---

# Troubleshooting

[Guide index](index.md) | Previous: [Security model](08-security-model.md) | Next: [Known limits](10-known-limits.md)

Entries marked "seen in lab" were observed in experiments E4-E6 (see `tests/p0/results/`); others are derived from the code or API contract.

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| API answers `409 settings_incomplete` | Installation ID, volume base, agent image or backup settings missing in the manager `settings` | Complete the `manager.settings` configuration and restart. |
| Agent tasks `Rejected`: `bind source path does not exist: /var/lib/dbcm-agent` (seen in lab) | Swarm does not create bind sources | Create `/var/lib/dbcm-agent` and the volume base on every host. |
| `refused (not-owned): referenced network/secret ... not in allow-list` (seen in lab) | The network or secret lacks the label `dbcm.installation=<installation ID>`, or the agent service was created before the secret was listed in `agentTransport.dbSecrets` | Add the label, list the secret, redeploy the agents. |
| Service commands fail on a worker: `This node is not a swarm manager` | No enrolled manager node with an agent | Enroll a Swarm manager node and deploy the agent there. |
| Pull errors, tasks stay `preparing` | Image absent on a node, no registry | Load or pull the image on every node. |
| Setup token lost | Token only printed at start | Restart while no administrator exists; read the new token from the log. |
| No passkey prompt, or origin mismatch | `rpId` or `origins` do not match the HTTPS URL you use | Fix the config; passkeys need the exact HTTPS origin. |
| All passkeys lost | - | Redeem a recovery code (`/auth/recovery/code`), then register a new passkey. There is no command line to issue recovery URLs yet. |
| API answers 503 with `X-DCM-Leader` | This replica could not reach an active leader, or forwarding is not configured | Check the leader, peer mTLS and port 7447; inspect operation state before retrying an uncertain write. Three plain manager processes ran on lab VMs; the HA stack and forwarding listener have not. If no replica leads and the log says it cannot unwrap the data key, the replica is not registered in the keyring yet: see the [runbook](11-operator-runbook.md#5-replicas-key-ring-and-peer-certificates). |
| Operation `paused` or a step keeps polling | An unmet precondition (for example a node not `Synced`) | Read `progress` in `GET /api/v1/operations` (the Operations page shows the waiting reason and, in red, the step's last error), fix the cause, then `POST /api/v1/operations/{id}/resume`. Abort works at the next poll or step boundary (`.../abort`). |
| `409 busy` or `resource locked` | Another operation holds the cluster | Wait, or abort the unfinished or failed operation first. |
| `409 requires_guided_recovery` on replace | Quorum or a verified donor is not observed | Use the [full-outage recovery](07-full-outage-recovery.md) path; there is no override. |
| `403` on approving a full-outage recovery | The `recover` permission is held by the administrator preset only | Approve as an administrator. |
| Replicas cannot reach each other with `peerTLS` | Wrong installation ID, wrong replica ID for an address, certificate of another CA, or a revoked serial | Compare `installation`, `peers[].id` and the certificate identity `dbcm://<installation>/metastore/<id>`; reissue with `-issue-peer-certs`. |
| `409` on entering host maintenance | A cluster would lose its majority outside the host, or an operation is unfinished | Read `GET /api/v1/hosts/{id}/maintenance-plan`; nothing was changed. |
| `409 approval_mismatch` | The digest differs from the stored plan | Re-read the operation's `approval.digest` and approve that. |
| Cluster create failed and the name is taken | A failed create keeps name and capacity | Delete the cluster (approval, administrator only), see [Creating a cluster](03-create-cluster.md). |
| Backup fails with `docker-error` on a worker | Job created by a non-manager agent (seen in lab, fixed) | Ensure a manager-node agent is enrolled. |
| Backup warning `galera_info not found` | MariaDB 11.4 does not write it to `--extra-lsndir` (seen in lab) | Informational; the position may be read from the stream (fakes only). |
| Proxy: about two aborted handshakes per second per node in node logs | Proxy health probes | Expected; one log line each (E5). |
| After a writer failure no write works for about 6 s | Galera failure detection (E5) | Expected; the proxy itself switched within 0.2 s. |
| A backup stays at step `wait-backup` with `manifest not written yet`, the job service `dbcm-job-...` shows `0/1` | The job exited and could not write its status (target unreachable, seen in lab E14); the operation has no signal for a dead job | `docker service logs dbcm-job-<id>` on the manager node shows the cause. Fix the target, abort the operation, start a new backup. |
| A backup waits with `backup job failed: ... 403 SignatureDoesNotMatch` or `507 XMinioStorageFull` | Wrong S3 credentials, or the target is full (seen in lab E14) | The reason names the cause. Correct the backend secret or free space, abort the operation, start a new one. |
| An aborted backup leaves a service `dbcm-job-...` at `0/1` | Abort stops the operation; it does not remove the job service (seen in lab E14) | Remove it after reading its logs: `docker service rm dbcm-job-<id>` (only that name). |
| Redundant proxies show `writer: none` and `coordination.pending` | A coordinated switch is frozen while at least one configured proxy has not acknowledged | Check each proxy's `/status`, manager connectivity and local Galera health. Restore the unreachable proxy; do not remove its membership merely to clear a partition. |
| `429` or "Too many attempts" when signing in | The per-address limiter: 5 unfinished or failed sign-in attempts in 15 minutes lock that address for 15 minutes (doubling up to 24 hours). Completed sign-ins reset the counter | The UI shows the remaining time. Wait for it; do not retry in a loop (every retry while locked is rejected). Behind a reverse proxy all users share the proxy address. |
| Abort of a rolling restart, minor upgrade or scale-in answers `409` | The operation stopped a database service (scaled it to 0) and has not started it again; aborting would leave the node down (seen in lab E13) | Resume the operation so it starts the node, or start the node by hand (`docker service scale <service>=1` on the manager node), then abort. |
| An operation shows state `unknown` | Its start was refused before anything ran (for example another operation held the lock) | Abort it on the Operations page; this only closes the record. |

## Collecting evidence

Manager log (stderr or `docker logs`), `GET /api/v1/operations`, `GET /api/v1/events`, `GET /api/v1/clusters/{id}`, and on a node `docker service ps --no-trunc <service>`. Do not run `docker system prune`, and do not delete volumes by hand: a volume's identity marker protects against starting on foreign data.
