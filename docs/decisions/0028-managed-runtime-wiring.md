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
- A server that is not yet a Swarm node has no host record and therefore gets no enrollment token; the native agent step of the bootstrap fails until the node is enrolled. Closed by the owner decision of 2026-11-07 (pre-issued token, see the addendum below; fakes only).
- Creating a metastore backup still needs `settings.backup.recipientKey` (409 `settings_incomplete` otherwise).

## Addendum 2026-11-07: pre-issued enrollment token for a not-yet-enrolled host

Owner decision: the enrollment-token gap found in E19 ([result](../../tests/p0/results/e19-copy-archive-bootstrap.md), part 3) is closed by **pre-issuing** the token. Fakes only; nothing ran against a VM.

| Question | Chosen mechanism | Alternative and reason |
| --- | --- | --- |
| When | `BootstrapService.Run` asks `PlannedEnrollment.Preissue` for the planned host before the state turns `running` and before the first step, but only if the plan has a step that takes the token on stdin. A failure returns an error and leaves the draft `approved`; the host is untouched. | At draft or plan time: the token would live (sealed) for the whole review and survive a failover without need. At run time it is held in memory for that run only and never written to the draft store. |
| Binding | Single-use token (two-hour TTL, digest only in the store) whose allowed hosts are exactly the draft's host ID. Because the PKI keys every identity (certificate, serial, report) on a host record, a placeholder record `bootstrap/<host id>` (`manager.PlannedSwarm`, enrolled, address from the draft if it is an IP) is created in the same transaction. Swarm sync and deployment ignore that scope. A token of that scope cannot enroll a registered host (swarm mismatch) and a registered token cannot enroll the planned host. | Letting `ConsumeToken` enroll without a record: every later step (signing, serial, report, patch directory) needs one. |
| Registered host | If an enrolled record with this node ID exists, `Preissue` returns "" and the executor falls back to `HostEnrollmentTokens` as before. A known node that is only discovered or revoked is refused: a placeholder would shadow it. | |
| Delivery | Unchanged: stdin of the `agent-native` step, never an argument, plan, view, draft document, stored secret or log. | |
| Failover and resume | Nothing is persisted. Every `Run` (also on a new leader, also a resume) withdraws earlier unused planned tokens of that host and issues a new one. `Release` is idempotent. | Persisting the token sealed: more secret at rest for no gain, since re-issuing is cheap and safe. |
| Wipe | `finishWipe` (cancel, fingerprint mismatch, expiry, completion) deletes the secrets document and then calls `Release`: unused planned tokens are revoked, a placeholder without an agent identity is deleted; one whose agent enrolled is that agent's record and stays. A failed release keeps the wipe pending and the sweeper retries it. | |

Alternatives considered:

- **Reorder the steps** (Swarm join first, native agent after, enrolled through the node record): the join makes the preflight refuse a second bootstrap of the same host, the node ID is only known after the join, and a Galera-less plain Docker host (no Swarm) would still have no record. Rejected.
- **Manual token** (the operator creates a token in the UI and types it into the draft): it would put a secret into a form and a draft, needs a host record anyway, and the operator would have to copy it between two screens. Rejected.

Consequences and limits:

- The placeholder appears in the host list with scope `bootstrap`. The host ID is the name the agent enrolls under; the container-agent step still needs a host ID equal to the Swarm node ID, so combine it with the native agent only when the IDs match.
- A token for a host whose address is not an IP cannot pass the source-address check when `RequireSourceAddr` is on.
- Fakes only: the manager tests enroll through the real enrollment server and PKI; the bootstrap tests use a simulated host. No lab run.
