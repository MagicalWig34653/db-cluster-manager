# E13 release-candidate lab run (2026-10-30, partial)

Three Lima VMs (arm64, 2 vCPU / 4 GiB / 20 GiB each), Docker 29.9.2, merged `integration` (a5f01a1) plus the fixes below. Host load 9-49, 67-73 GiB free disk at start. VMs removed with `bin/lab cleanup --apply` (3 removed, 3 unrelated resources intact).

## Executed and passed

- `tests/p0/rc-demo.sh up` (E12 phases build, vms, swarm, images, prereqs, hainit, hastack): three HA manager replicas running, leader elected, follower 503 + X-DCM-Leader. About 4 minutes; the setup token was printed. `forward` (ssh tunnel to localhost:8080) worked.
- Real Chromium (Playwright, virtual WebAuthn authenticator) against the real manager: administrator setup with recovery codes, passkey login, connect Swarm, sync, enroll three hosts, deploy agents (3 labeled), create a 3-node Galera cluster through the wizard (23 of 23 steps, cluster healthy), plan a rolling restart, approve its digest, and watch it finish (28 of 28 steps, run after the fix below), abort of a paused operation with confirmation.
- 360 px: no horizontal overflow on hosts, clusters, cluster detail, operations, wizard. Keyboard: tab order reaches the wizard fields in a sensible order.
- Real screenshots: `docs/assets/screenshots/real/`.

## Bugs found and fixed test-first

1. Wizard sent a cluster without CPU/memory limit; the planner answered a bare `400 bad_request`. Validation and hint text added (`web/src/lib/cluster.ts`, i18n EN/DE, vitest).
2. Rolling restart failed at `await-stopped` on the first answer because the stopping task was still counted. `await-stopped` is now idempotent and polled (`internal/runner/types.go`, test).
3. After that failure a new leader adopted the operation as paused but it kept `EndedAt`, so `unfinishedFor` allowed planning a second operation that then sat in `unknown` on a runner lock. Fixed in `internal/manager/operations.go`, test added.

## Observations, not fixed

- The login limiter (5 failures in 15 minutes) locked my scripted logins out after rapid repeated sign-ins; the 429 shows as a generic error.
- The operations page lists an `unknown` operation that cannot be aborted through the UI (abort answered 404 for it); it stayed until the cluster operation finished. Not investigated further.
- The first rolling-restart attempt cost one stuck cluster node (service scaled to 0); I scaled it back by hand with `docker service scale`.

## Not run

- Stage 3: backup now, restore into a new target, PITR, local and MinIO targets, copy to a second target. They need a manager reconfiguration (backup settings, backend secret, recovery key) and a MinIO setup that I did not do.
- The scripted fresh-user INSTALL.md walkthrough: `rc-demo.sh` reused the E12 harness, which is not the manual document path. No documentation gaps were collected.
- Scale-out approval, passkey on non-localhost origins, Firefox/Safari.
