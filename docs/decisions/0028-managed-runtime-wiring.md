# ADR 0028: Managed runtime wiring of composer, bootstrap and SSH install

Status: **Implemented with fakes only (2026-10-30); nothing ran against a VM, a real host or a registry.** Closes the 503 gaps left by [ADR 0026](0026-image-composer.md), [ADR 0027](0027-fresh-server-bootstrap.md) and [ADR 0023](0023-os-patch-management.md) in the managed manager runtime.

## Decision

| Question | Chosen mechanism | Alternative and reason |
| --- | --- | --- |
| Switches | `manager.imageComposer.enabled` and `manager.serverBootstrap.enabled`, both default off; routes answer 503 while off. The shipped example and `manager init` carry both with `enabled: false`. | `manager.bootstrap` was requested but is already the boolean that starts the metastore cluster; the new key is `serverBootstrap`. |
| Composer | `api.ImageComposerBackend` on the image build service. Needs `auth.enabled` and `manager.imageBuild` (validated at start). Without a docker builder the endpoints stay 503. | Own build path: rejected, builds keep the approval, digest and idempotency flow of X08. |
| Draft store | `provision.DraftStore` on the replicated metastore, sealed with the data keyring (`manager.BootstrapDraftStore`). The draft document (plan, pinned host key, state) holds **no credential**; the credentials (root/dedicated login, join token, expiry) are a second sealed document `bootstrap-secrets`. Wiping is deleting that document. | One document: a failed wipe could not be told apart from a failed state write, and every state change would rewrite the credential. |
| Expiry and wipe | One-hour lifetime stored with the credentials; enforced on every read, on restore and by a leader sweeper (`manager.DraftSweeper`, `sweepSeconds`, default 60). A failed deletion stays pending and is retried. Deleted after the run, on cancel and on a fingerprint mismatch. | Expiry only in memory: a restart would resurrect expired credentials. |
| Leader fencing | `Term()` returns 0 on a follower: `Run` is refused (`ErrNotLeader`). The executor journals under the term; a run that finishes after the term changed records nothing (the new leader owns the draft). | |
| Failover | The running state is saved before the host is touched. `Restore` marks a stored `running` as `unknown`, restores succeeded steps into the new journal under the new term, and `Run` resumes (the host check runs before each step is repeated). | Auto-resume: rejected, an interrupted mutating step needs the operator's decision. |
| Enrollment tokens | `manager.HostEnrollmentTokens`: a single-use token (two-hour TTL, not stored in plain form) limited to one host, issued only for a host with an enrolled host record whose node ID equals the bootstrap host ID. Used by the bootstrap (`TokenSource`) and the patch installer. | A token for any ID: rejected, it would let an unknown host enroll. |
| Container agent | `manager.BootstrapManagerActions.DeployContainerAgent` enrolls the Swarm node (never a revoked one) and calls the existing `Swarms.DeployAgents` as the manager system subject; the dedicated key is sealed in `bootstrap-host-keys`. | A second deployment path: rejected. |
| Patch SSH install | `patch.Installer` on `manager.PatchInstallSealer` (sealed, replicated), drafts expire after one hour (`Installer.Sweep`), the credential is deleted after any attempt that reached the host. | |
| Backup copies | No code gap found: `Backups` implements `api.BackupTransfers` and `api.MetastoreBackups`; a test starts the real runtime with a `targets` entry and reaches the routes. | |

## Known limits

- Fakes only. The sealed round trip is tested on a one-replica in-memory Raft cluster and through the real runtime restarted on the same data directory; a failover between two real manager processes was not run.
- The step journal is still in memory; after a restart only the steps stored as succeeded are known, and the step in flight is re-checked on the host.
- A server that is not yet a Swarm node has no host record and therefore gets no enrollment token; the native agent step of the bootstrap fails until the node is enrolled. Closing this needs a product decision (a token for a not-yet-known host).
- Creating a metastore backup still needs `settings.backup.recipientKey` (409 `settings_incomplete` otherwise).
