---
title: Agent runtimes and capabilities
---

# Agent runtimes and capabilities [Fakes and loopback only; not run on VMs]

Two kinds of agent exist ([ADR 0024](../../decisions/0024-agent-runtimes.md)):

| Runtime | What it is | Capabilities |
| --- | --- | --- |
| `container` | The Docker agent, a Swarm global service. Default for everything that touches Swarm objects. | `docker-objects`, `galera-steps`, `restic` (only with allowed paths), `diagnostics` (reserved) |
| `native` | A systemd service on the host without Docker access ([guide 23](23-patch-management.md)). | `patch`, `restic` (only with allowed paths), `host-maintenance` and `diagnostics` (reserved, unused) |

A report that lists a capability its runtime cannot have (for example a container agent claiming `patch`) is rejected: at enrollment with HTTP 400 `runtime-mismatch`, afterwards the host shows `agentReportError` and is refused for every operation until a correct report arrives.

## Which runtime runs what

| Step type | Capability | Runtime |
| --- | --- | --- |
| Services, volumes, configs, secrets, backup jobs, deny list (`create-service`, `remove-volume`, ...) | `docker-objects` | container only |
| Identity markers, volume directories, node health, SQL templates, recovery steps (`init-identity`, `exec-sql`, `collect-health`, ...) | `galera-steps` | container only |
| `restic-backup`, `-snapshots`, `-forget`, `-check`, `-restore`, `-stats` | `restic` | native preferred, else container |
| `patch/report`, `simulate`, `apply`, `reboot`, `deny-list` | `patch` | native only |

The table lives in `internal/agentrt`. The manager never falls back silently: if the required runtime is not reported by the host, the operation is refused with the reason ("capability patch needs a native agent; the host reports container [docker-objects,galera-steps]"). `POST /api/v1/host-sources/{id}/backups` then answers 409 `agent_runtime_unavailable` and starts nothing. A host whose agent never reported (older build) counts as a container agent without `restic`; update the agent so it reports.

## Where you see it

- `GET /api/v1/hosts`: `agents` lists each recorded runtime, capabilities, whether a current certificate is recorded (`identity`), and its `certificateExpiresAt`. `agentReportError` is set after an inconsistent report. The Hosts page shows one badge per runtime (UI gap: no per-function view yet).
- `GET /api/v1/host-sources[/{id}]`: `runtime` is the runtime that would run the source now (native if it offers `restic`, else container), `runtimeNote` the reason when none can.
- `GET /api/v1/operations`: `executedBy` lists per step the `runtime`, the `host` and the `agent` identity that executed it. The operation timeline has `[executed by runtime=native host=...]`.
- Patch: `agentRuntime` on the patch host, `executedBy` on a plan.
- Audit: one record per executed step (`operate` on the host) naming operation, step, runtime and agent identity.

## Restic on the native agent

Add `resticPaths` to the install request of the patch agent ([guide 23](23-patch-management.md)); the plan is reviewed and approved as before. The unit then starts `dbcm-agent patch ... -restic-allow-path /srv/data -restic-secrets-dir /etc/dbcm-patch/secrets`, the script creates the secrets directory (mode 0700) and stops if `restic` is not installed. Install `restic` from your distribution or the pinned release first; the preflight shows `restic` and `ssh` (needed for SFTP repositories). By hand, start the agent with the same flags.

The rules are those of [guide 22](22-restic.md): the agent backs up only below the allowed roots, never the secrets directory or the Docker socket, reads the password and backend credentials from regular files named by the source (`passwordSecret`, `credentialSecret`) inside the secrets directory (create them by hand, mode 0600, owned by the agent user), restores only into a new path with approval. Report-mode agents run as the unprivileged user `dbcm-patch` and can read only what that user can read; for whole-host backups install the apply mode (root) or grant the user read access to the allowed paths.

## Limits

- Container and native agents may enroll on the same host with the same enrollment token, once per runtime. Each has its own certificate and renewal cycle. Existing single-slot hosts remain container agents. Host unenrollment revokes both certificates. The manager pins the runtime URI and current serial and delivers the certificate deny list to both runtimes separately ([ADR 0024](../../decisions/0024-agent-runtimes.md)).
- An administrator can revoke one runtime's certificate with `POST /api/v1/hosts/{id}/agents/{runtime}/revoke` (`runtime` is `container` or `native`); the host and the other runtime stay enrolled. The revoked agent needs a new enrollment token. The existing `POST /api/v1/hosts/{id}/revoke` revokes the whole host and both certificates.
- A native agent enrolled by the older single-slot build holds the legacy container URI. It needs a fresh native enrollment token and certificate before this manager will route to it; its old certificate cannot renew as native.
- Tested only with fakes and loopback listeners; no VM and no real restic run on the native agent. The secrets directory of the native agent is not filled by the manager.
- `host-maintenance` and `diagnostics` are reserved names; nothing is routed to them.
