# E17 part d: restic through the real manager and the native agent against MinIO over TLS (ADR 0022, X14)

Run: 2026-10-31, same lab as part c. E11 exercised the restic wrappers through a harness (`cmd/resticlab`); this run goes through the product path: module switch, host-source API, fenced operation, **native agent on the host** (m1, apply mode, `-restic-allow-path /srv/e17data`), restic 0.19.1 linux/arm64 (SHA-256 checked against the pin of `deploy/Dockerfile`). Backend: MinIO (cross-built, same module version as E11) on m2 with a lab-CA certificate for the VM IP, bucket created by `restic init` from the operator side. Password and S3 keys are synthetic, lab-own, held as 0600 files in `/etc/dbcm-patch/secrets` (the native agent's secret directory) and as sealed values in the manager; none is recorded here.

## What ran (real)

| Step | Result |
| --- | --- |
| Source API while the module was off | `404 module_disabled`; after `PUT /modules/host-backups {"enabled":true}` `200` and an empty list |
| `POST /host-sources` with an `s3:http://...` repository | **`400`**: by design only `https` (or scheme-less, which restic treats as https) S3 locations are accepted. MinIO was then given TLS |
| `POST /host-sources` (agent transport, `s3:https://192.168.104.3:9000/e17-restic`, keepLast 3 + prune, `checkSubset 10%`) | `201`; `runtime: native` computed |
| Backup operation (first two attempts) | **failed `repository locked`** - a product bug, found and fixed (below) |
| Backup operations after the fix | succeeded: `backup`, `retention` (`forget --keep-last 3 --prune`), `check` steps all `executedBy runtime native, agent/<host>`; the fourth snapshot made the retention remove the oldest one (3 left); the exclude (`/srv/e17data/src/cache`) is absent from the snapshot |
| Restore of an earlier snapshot to a new path | restore operation waits for approval (`pending`, digest); approve with a wrong digest `409 approval_mismatch`; approve with the right digest -> succeeded |
| Verification | `sha256sum -c` over the restored tree: all 7 files OK, mode `640` and the symlink preserved, no `cache/`; the **latest** snapshot restored to a second path matches the second state (changed, added and deleted files), the first restore tree was untouched |
| Allow-list | restore to `/etc/e17-evil` (outside `/srv/e17data`) was approved on purpose and the agent **refused** (`refused (invalid-path): restore needs approval and a new allowed target`); a restore onto the already existing first target was refused the same way |
| Repository integrity | `restic check --read-data` from the operator side: all 6 packs read, no errors |
| Secret hygiene | no process argument and no journal line of the agent or the manager contains the password or the S3 secret |

## Bugs found by the real run (fixed with tests)

1. **Apply-mode native agent could not run restic at all.** The apply unit is `User=root` with `ProtectHome=true`; restic's default cache is `$HOME/.cache` = `/root/.cache`, read-only there: `unable to open cache: mkdir /root/.cache: read-only file system`. The container agent gets `RESTIC_CACHE_DIR` from its deployment, the native agent got nothing. The agent now sets `RESTIC_CACHE_DIR` to `<state dir>/restic-cache` (0700) when the capability is on and the variable is unset (`TestEnsureResticCacheDir*`). Report mode set `HOME` to the state directory and was not affected.
2. **The error was reported as a locked repository.** The wrapper classified any stderr that contains the substring `lock` as `ErrLocked`; restic's Go stack trace for that failure contains `cmd/restic/lock.go` and `OpenRepository`. The message told the operator to inspect a stale lock that did not exist, and the failed operation kept the source's resource lock (a later backup answered `unknown ... resource locked by another operation` until the failed one was aborted). `classify` now matches restic's phrases (`already locked`, `unable to create lock`, `failed to lock`, exit status 11) and the repository case no longer matches bare `repository` (`TestClassify*`).

## Findings (not fixed)

- A failed restic operation keeps `host-source/<id>` locked until an operator aborts it (`POST /operations/{id}/abort`); the next backup then waits with state `unknown`. Intended (stale-lock caution), but after a plain failure like bug 1 it needs a manual step that the message does not mention.
- The agent hides restic's stderr by design; the cause was found by wrapping the `restic` binary on the host (the unit has `PrivateTmp`, so the wrapper log lived in the unit's private `/tmp`).
- The apply-mode unit's `ProtectHome=true` makes `/home` paths unusable for backups and restores; the install plan only sets `ProtectHome=read-only` plus `ReadWritePaths` for report mode. Not changed; sources below `/home` need the report-mode unit or a unit edit.
- Repository initialisation, the Swarm-secret-free native secret files and the S3 bucket were created by hand (documented as operator steps).

## Not verified

SFTP, rest-server and local backends through the manager (E11 covers the wrapper); the container agent path; the restic schedule (`schedule`); retention across many runs; the external-host SSH transport; restores over live data (refused by design); wrong-credentials and unreachable-repository behaviour through the manager (E11 covers the agent side); the UI (there is no UI for host sources).
