# E17 part c: patch management with the real manager, SSH install, enrollment and mTLS (ADR 0023, X15)

Run: 2026-10-31 on the same two lab VMs as part a (m1 Debian 12, m2 AlmaLinux 9.8, both moved onto the `lima:user-v2` network for VM-to-VM traffic; Swarm re-initialised on m1 with m2 as worker, because patch host IDs are Swarm node IDs). The **real manager** (`manager` cross-built for linux/arm64 with the embedded UI, managed single-instance mode, `auth.enabled`, `agentTransport`, `patch.enabled`, `pollIntervalSeconds 60`) ran on m1 as a lab-own systemd unit. The first administrator was created through the real WebAuthn endpoints with the software authenticator of `tests/p0/e4client` (no browser). Unlike E10, nothing stood in for the transport: SSH installation, enrollment and mTLS are the product paths.

## What ran (real)

| Step | Result |
| --- | --- |
| Swarm connect, sync, `enroll` of both nodes | worked (host records `enrolled`) |
| `POST /patch/install/plans` for m1 (root, key, mode `apply`, `resticPaths`) and m2 (user `dbcm` with sudo, dedicated key from part a, mode `report`) | worked; the plan came from a real SSH preflight with pinned host key. The agent binary was served over **HTTPS from the host** (lab-own CA installed in both VMs' trust stores) and its SHA-256 was checked on the host |
| `POST .../execute` with the plan digest | `installed` on both. Units `dbcm-patch-agent` active: m1 `User=root`, `-allow-apply`; m2 `User=dbcm-patch`, sandbox. Journal: `agent: enrolled` within one second, `patch agent started with the mTLS transport`, listening on 7445 |
| `PUT /patch/hosts/{id}` + `POST /patch/hosts/{id}/poll` (m1) | **mTLS poll works**: Debian 12.x, kernel, `apt`, 17 updates, all 17 from `oldstable-security` classified security, `securityKnown true`, reboot `no` |
| dnf scan (m2) | scheduler poll after 60 s: **76 pending, 64 security**, `AlmaLinux 9.8`, advisory IDs from `dnf updateinfo` (`ALSA-2026:58572`, ...). The manual `poll` request took about 90 s (dnf metadata as the unprivileged user); the first manual call hit a 60 s client timeout and the manager logged `context canceled` as a poll error - the next scheduler poll succeeded |
| **Container-runtime class** | after `dnf downgrade docker-ce docker-ce-cli` to 29.9.1 on m2, the next poll reported `docker-ce 3:29.9.2` and `docker-ce-cli 1:29.9.2` with `class: container-runtime`, the other 76 as `os` |
| Engine plan under the default policy | plan creation works (`containerRuntime: true`, dnf update-list simulation), the engine guard says `no engine runtime source is configured` (known limit), **approve is refused `409 policy_conflict`** (report-only). A plan mixing `docker-ce` with an OS package: `400` |
| OS plan on m1 under the default policy (`report_only`) | approve refused `409 policy_conflict` |
| Policy `approval_required`; plan `bind9-host`+`bind9-libs` on m1 | apply before approval `409`; approve with a wrong digest `409 approval_mismatch`; approve and apply -> **succeeded** through the apply-mode agent (`executedBy native`), apt simulation `native`; history shows the `report` diff (`clearedUpdates`, `installedChanged 1:9.18.49-1~deb12u2 -> deb12u3`) and an `apply` entry; pending dropped 17 -> 15 |
| Same on m2 (report-mode agent) | plan and approve succeed (policy is the manager's), **apply -> plan `failed`**: the report-mode agent refuses |

## Findings (not fixed unless stated)

- **Install preflight failures are an opaque `409 conflict`.** My first plan was refused because the restic binary was missing on the host (documented requirement); the API answers only `{"error":"conflict"}` for a missing restic binary, an SSH authentication failure and an existing agent alike. Error codes are coarse by design (ADR 0030) but an operator cannot tell the cases apart.
- **The enrollment token file stays on a report-mode host.** After enrolling, `/etc/dbcm-patch/enroll-token` (owned by `dbcm-patch`, mode 0600) still exists on m2 because the agent user cannot delete from the root-owned directory; on the apply-mode host (root) it was removed. The token was single-use and already consumed, so the exposure is nil today, but a stale credential file is untidy.
- **How an operator learns the CA fingerprint is not documented.** The install request needs `caFingerprint`; I read it from the TLS chain of the enrollment port (`openssl s_client -showcerts`, second certificate). `manager init` output was not part of this run.
- The apply-mode unit is `User=root` with `ProtectHome=true` (see the restic part: this broke restic until the cache directory was fixed).
- The manual poll endpoint blocks for the whole dnf run; clients with a 60 s timeout abort it.

## Not verified

Reboot execution and `reboot-required` after a real kernel update (no kernel package applied); the Galera maintenance guard against a real cluster; the engine apply path (the guard has no engine runtime source in the shipping manager); manager failover during an apply; the install over a host whose agent exists (refusal is unit-tested); yum, zypper, apk, pacman; the UI (there is none). The agent binary was a lab build served from the host over a lab-own CA, not a release artifact.
