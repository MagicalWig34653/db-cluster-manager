# ADR 0021: Backup copies, encrypted targets, scheduled verification and metastore backup

Status: **Proposal implemented with fakes**, 2026-10-26. Extends
[ADR 0007](0007-backup-transports.md) and [ADR 0009](0009-cluster-backup-method.md).
Nothing here ran against a real S3 service, SFTP server, Swarm or MariaDB.

## Confirmed requirements

[KONZEPT.md](../KONZEPT.md) requires backups to local, S3-compatible and SFTP
targets with client-side encryption, retention, verified restores, copies
between clusters and Swarms **without** automatic promotion, and a backup of
the manager's own metadata with a documented recovery path. The rules in
[AGENTS.md](../../AGENTS.md) stay: no hidden promotion of a cross-cluster copy,
full-cluster recovery needs explicit approval.

## Decisions (proposed)

1. **Signed documents.** `backup.Sign`/`Signed.Verify`: HMAC-SHA256 over format,
   purpose, key ID and payload. The MAC key is HKDF-derived from a manager data
   key (the keyring), per purpose, so a transfer record never verifies as a
   metastore record. Alternative considered: Ed25519 signatures with a manager
   signing key. Rejected for now: a second long-lived key to distribute and
   rotate; the HMAC is verifiable by every replica that holds the data key,
   which is also the limit (only keyring holders can verify; documented).
2. **Client-side target encryption.** `backup.EncryptedBackend` wraps any
   `Backend`: 64 KiB AES-256-GCM chunks, per-object salt, key HKDF-derived from
   a keyring data key, key ID in the header (rotation keeps old objects
   readable), name, chunk counter and a final flag in the AAD (swap, reorder,
   truncation and extension fail). Configured in the backend config
   (`encryption`: key ID, base64 key, previous keys). Cluster data streams are
   already sealed for the recovery key; this layer protects manifests, status
   files, transfer and metastore records and gives non-stream objects the same
   confidentiality. Alternative: encrypt only in the manager. Rejected: jobs
   write to the target themselves. Limit: the key sits in the backend config
   secret, as the S3/SFTP credentials do.
3. **Copies.** `dbbackup.CopyBackup` streams stream, manifest and a signed
   transfer record from one `Backend` to another, checks ciphertext digest and
   size against the manifest while copying (a mismatch deletes the copy), writes
   the manifest and then the record last. `VerifyTransfer` checks signature,
   manifest digest and fields, and (deep) the stored object. Retention of the
   source set deletes its copies (record first). The copy is data only.
4. **No automatic promotion.** A restore from a copy starts with
   `ProposeTransferRestore`: deep verification, a digest-bound
   `RestoreProposal` for a NEW cluster (`approval_required=true`,
   `auto_promotion=false`). `Approve` requires the exact digest and a named
   approver; `TransferRestoreRequest` refuses without it and
   `ClusterRestorePlan` still refuses the source identity and still requires
   the approval on every destructive step. Nothing schedules a restore from a
   copy.
5. **Scheduled restore verification.** A policy field `verify` makes the
   leader run the existing isolated inspection restore on a schedule. Enabling
   it needs the *restore* permission and is recorded as the approval of the
   scheduled isolated inspections (never a full restore). Failure, abort,
   overdue or not-started runs record an event and notify
   (`backup.restore_verification_failed`). Alternative: approve each run.
   Rejected: a verification nobody approves at 04:00 never runs.
6. **Metastore backup.** The leader exports the documents that describe the
   installation (not sessions, operations, approvals, events) and seals the
   export for the public half of the backup recovery key with the stream
   format; a signed record is written last. The running manager cannot read
   its own backups. `manager metastore-import` restores offline into an empty
   data directory with the recovery key from a file; the keyring is written
   first and its data keys are re-wrapped for the importing replica. Other
   replicas are re-added through membership. Alternative: Raft snapshot files.
   Rejected as the only mechanism: they are not encrypted for an external
   key and carry sessions and operations.

## Consequences and limits

- Evidence: fakes, `httptest` S3, an in-process SFTP server, in-memory Raft.
  No VM run. Tracked in [STATUS.md](../STATUS.md).
- Transfer verification needs the keyring of the installation that wrote the
  record; another installation cannot verify it. A cross-installation trust
  anchor is a future decision.
- The export includes sealed secrets; it is as sensitive as the store. The
  recovery key must be kept outside the manager.
- Imported state can lag reality (hosts, services, clusters changed since the
  backup). The import brings the manager up in the state of the backup; the
  runbook requires reconciling against live Swarm state before any operation.
