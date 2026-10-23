# ADR 0009: Cluster backup method, restore isolation and verification

Date: 2026-10-22
Status: Proposed. Implemented as planning code, job helper and agent commands in `internal/dbbackup`, `internal/agent` (`run-backup`, `run-restore`, `remove-job`) and `internal/backup` (stream format). No real mariabackup, mbstream, mariadb-dump or MariaDB server has run against this code. Reopen after the first lab restore (see "Not verified"), or when PITR becomes a release gate (R3).

## Context

Requirements B01-B06 and the R1 gate "verified full restore" need cluster data backups to S3/SFTP, a full restore, opening a backup in an isolated temporary database (B03, R3), selective restore with preview and approval (B04, R3), configurable restore placement with limits (B05) and restore verification (B06). [ARCHITEKTUR.md](../ARCHITEKTUR.md) adds: temporary restored databases must not join production Galera membership, restart upstream replication or expose production credentials by default; selective restore must handle schema/dependency conflicts and target write activity. Backup success and verified restorability are distinct states ([KONZEPT.md](../KONZEPT.md)).

## Options

| Option | For | Against |
| --- | --- | --- |
| Physical only (`mariabackup --backup --stream=xbstream`) | Fast for large data, consistent at an LSN, full-cluster restore needs no SQL replay, records `xtrabackup_checkpoints`, Galera position (`--galera-info`) and binlog position (base for PITR). The same tool Galera uses for SST. | A single database cannot be restored without restoring the whole instance first; restore needs the same major server version; prepare step needed. |
| Logical only (`mariadb-dump --single-transaction`) | Per-database export, version tolerant, can be loaded under another name and into a running cluster. | Slow for large data, restore replays SQL; no LSN or Galera position, so no base for PITR; long transactions on the source. |
| Both | Full restore and PITR base from physical; selective restore from per-database logical exports; table-level browsing from an isolated physical restore. | Two kinds of jobs and storage use. |

## Decision (R1)

1. **Full physical backup** with mariabackup, streamed as xbstream from a chosen node, and **per-database logical exports** with mariadb-dump (one job per database, without `--databases`, so the dump has no `CREATE DATABASE`/`USE` and can be loaded under another name). Both run as configured schedules; the logical export is optional per database.
2. **Restores**: (a) full restore only into a NEW cluster with a new identity; (b) isolated inspection restore into a temporary single-node instance; (c) selective restore of whole databases from logical exports, with a preview diff that the user approves. Table-level selection is done by opening the backup (b) and exporting from there (B04 browse/export); it is not a separate restore path in R1.
3. **PITR is a documented follow-up** (below), not part of R1.

## Galera source selection

Implemented in `SelectSource` and re-checked by the `backup-source-eligible` gate right before the job starts:

- The source must be `Synced`, healthy, in the Primary component, with the expected cluster size.
- Never a node in `Donor/Desynced`: it is serving an SST (or was desynced by an operator) and its data lags the cluster. No backup at all while any node is `Joiner`/`Joined`: a running state transfer must not compete with backup I/O.
- At least two `Synced` nodes, so a backup never loads the last serving node.
- Prefer non-writers; the writer is used only with `AllowWriter`. mariabackup's `BACKUP STAGE BLOCK_COMMIT` briefly blocks commits on the source node, which on the writer stalls clients.
- Nodes (and the cluster) above a flow-control threshold (`wsrep_flow_control_paused`, default 0.1) are rejected; the host must have free CPU/memory for the job's limits.

**Desync and flow control.** A backup reads the datadir heavily; the node's applier can fall behind and send flow-control pauses that throttle writes cluster-wide. Setting `wsrep_desync=ON` on the source for the backup's duration stops it from sending flow control, at the cost that it lags; if the lag exceeds gcache when desync ends, the node needs IST or SST; while desynced it is `Donor/Desynced` and must not be picked as SST donor; desync must always be reverted, also after a crash of the job. R1 does **not** desync: it picks a non-writer, rejects throttled clusters, and leaves flow control on so the cluster stays consistent. A desync option with a guaranteed revert step is a follow-up once lab measurements show throttling.

## Job execution model

The agent never execs into the database container and never takes an argv from the manager. `run-backup`/`run-restore` take structured arguments; the agent builds the job command line itself (`agent.BackupJobArgs`, `agent.RestoreJobArgs`) and launches an owned one-shot Swarm service (replicas 1, restart condition `none`, pinned to its own Swarm node ID, mandatory CPU/memory limits, labels `dbcm.job`, `dbcm.job-id`). A physical backup mounts the source volume read-only after the agent verified its identity marker. The job reaches the node over the cluster network by its service hostname. Credentials (DB option file, backend configuration, recovery key for restores) are Swarm secrets that must pass the ownership guard; they never appear in arguments. Inside the job, `dbbackup.RunJob` parses the argv strictly and runs only `mariabackup`, `mbstream`, `mariadb-dump` and `mariadb`. `create-service` and `update-service` refuse the `dbcm.job` label, so an ordinary service cannot pose as a job, and jobs cannot be updated. `remove-job` only removes services labelled as jobs.

A swarm `replicated-job` mode would be cleaner than replicas 1 with restart `none`; the internal Docker client does not support it yet.

## Encryption and integrity

- Stream format (`backup.NewSealWriter`/`OpenStream`): random 256-bit data key per stream, AES-256-GCM chunks (default 1 MiB), nonce = chunk index, AAD = header hash || index || final flag. Reordering, splicing between streams, truncation and trailing data are detected.
- Key scheme: the data key is wrapped for an X25519 public key derived (HKDF) from the externally retained recovery key of `internal/backup`. A scheduled backup job needs only the public key; the recovery key is needed for restores and is passed as a short-lived Swarm secret. This keeps "the manager never stores the recovery key" true for unattended backups.
- Streaming upload: `backup.PutStream` hashes and counts what the backend consumed. A failing tool closes the pipe with an error, so no backend commits a partial object (S3 aborts the multipart upload). The SFTP backend no longer retries an upload after bytes of the stream were consumed (it previously could have resent a partly consumed reader).
- Manifest (`<object>.manifest.json`, written last, so its presence marks completion): backup ID, method, cluster ID, source node UUID, tool and version, per-chunk digests and plaintext total, stored object size and SHA-256, mariabackup checkpoints (LSNs), Galera state UUID and seqno, optional catalog and marker. The manifest is not secret and not signed; every digest is checked against the authenticated stream on restore, so a tampered manifest makes a restore fail closed.

### Amendment (2026-10-23): keyed chunk digests (manifest format 2)

Problem (open question from lab E4): format 1 manifests stored the plain SHA-256 of every 1 MiB plaintext chunk and of the whole plaintext next to the encrypted object. Anyone with read access to the backend (without the recovery key) could confirm guessed content (a known table page, an empty or default database), see which chunks are equal across backups and therefore what changed between them.

Options considered: (a) drop plaintext digests and rely on AES-GCM tags plus the stored-object SHA-256 only; (b) encrypt or sign the manifest; (c) keyed digests derived from the per-stream data key.

Decision: (c). Format 2 records `mac` per chunk = HMAC-SHA256(K, chunk index u64 || plaintext) and `plain_mac` = HMAC-SHA256(K', plaintext), where K and K' are derived from the stream's random data key with HKDF-SHA256 (infos `dbcm-backup-stream-chunk-mac-v1` and `…-plain`). The data key is only reachable with the recovery key, and it is different for every stream, so digests neither reveal plaintext nor link two backups. Restores still compare every chunk and the total against the manifest after decryption (the reader computes both the keyed and the plain digests). (a) would lose the independent end-to-end check of the job's view of the data against the restore's; (b) needs a signing key in the job and adds key management without hiding more than (c). The stream container itself is unchanged (version 1); `cipher_sha256` and sizes stay plain because they describe ciphertext and are what the manager checks without the key.

Compatibility: `ManifestFormat` is 2; `Validate` accepts format 1 (plain SHA-256 only, no MACs) and format 2 (MACs only, no plain hashes) and refuses mixed manifests; `CompareSummary` compares on the digests the manifest carries. Format 1 backups written before this change stay restorable (tested with a rewritten manifest, including tamper detection). Evidence: unit tests only (`internal/backup` stream tests, `internal/dbbackup` manifest and job tests); no real backup was re-run.

## Restore isolation

- **Full restore** (`ClusterRestorePlan`): reuses the cluster-creation plan for a new cluster ID, name and network and inserts, between n1's identity marker and its first start, the restore job, a completion gate and the job's removal. Refused when the target cluster ID equals the backup's cluster or any live cluster, or when name, network, node UUIDs, volumes or service names overlap production; refused when the manifest's cluster differs from the requested one. The job removes `grastate.dat`, `gvwstate.dat` and `galera.cache`, so n1 bootstraps a new Galera history; the distinct cluster name and network keep production nodes out. The agent additionally refuses a restore into a volume mounted by any non-job service (a production node, running or stopped), into a volume whose identity belongs to the backup's cluster, into a non-empty datadir, or without an approval ID.
- **Inspection** (`InspectionRestorePlan`): a single node with `--wsrep-on=OFF --wsrep-provider=none`, an empty cluster address, cluster name `inspect-<session>`, port 3307, `--skip-slave-start`, events and binlog off, attached only to a dedicated session network. An `--init-file` secret (`RenderInspectionInit`) locks every restored account (`account_locked` in `mysql.global_priv`), clears replication configuration and creates a read-only `dbcm_inspect` account with a random password; production credentials stay in the data but cannot log in. Expiry (at most 7 days), resource limits and `TeardownPlan` (remove job, instance, volume after verifying the inspection identity; no approval, so expiry cleanup runs unattended) are mandatory. Host selection (`SelectInspectionHost`): approved hosts only, or automatic; fit CPU/memory and data size + 25 %, prefer hosts without production nodes (B05).
- **Selective restore** (`PreviewSelective`, `SelectivePlan`): the preview compares the backup catalog with the live target: existing schemas (blocker unless replace is chosen), tables that will be dropped and recreated, tables retained, replaced views/routines, foreign keys that would dangle (including references to a schema restored under another name), recent writes to the target (blocker unless acknowledged), and accounts (never created or changed; reported). The plan requires an approval whose digest equals the preview's digest; any change needs a new preview and approval.
- Every command step of a restore operation carries the `approved` condition, and the agent refuses `run-restore` without an approval ID. Backups need no approval.

## Verified restore

`EvaluateVerification` turns observations of a restored inspection instance into a report: stream digests matched, server ready with wsrep off, table count per schema equal to the catalog captured with the backup, a deterministic checksum sample equal to captured baselines, and the backup marker when one was written. Only a passed report updates "last verified restore" (`VerifiedStore.LastVerified`), which the UI shows per cluster. Retention never deletes the newest verified backup set, the newest set, or a set pinned by a session (`RetentionSelect` on top of `backup.Select`). Schedules use a five-field cron parser with an injected clock and a missed-run policy (`skip` or `run-once`, never a catch-up burst).

## PITR follow-up (not R1)

Requirements for a design:

- Binary logging on the nodes that may be the archive source: `log_bin`, `binlog_format=ROW`, and `log_slave_updates=ON` so a Galera node's binlog also contains writes applied from other nodes. Galera writes the same transactions to every node's binlog only with these settings.
- Consistent GTIDs across nodes (`wsrep_gtid_mode=ON`, shared `wsrep_gtid_domain_id`, distinct `gtid_domain_id`/`server_id` handling) so an archive can continue on another node after failover.
- A continuous archiver that ships closed binlog files to S3/SFTP, tracks gaps visibly and survives node replacement; the starting point is the binlog position mariabackup records with the physical backup.
- Restore: physical base, then `mariadb-binlog` replay to a time or GTID into the isolated restore, never into production.
These settings change the production node configuration and disk use; they need their own decision and lab measurements.

## Verified and not verified

Verified by `go test -race -count=3 ./internal/dbbackup/... ./internal/agent/... ./internal/backup/...` with fakes: stream format round trips and tamper cases; PutStream on Dir, Mem, fake S3 (multipart, abort) and in-process SFTP (no retry after a dropped connection mid-upload); source selection rules; plan shapes accepted by a real agent executor; agent guards for the job kinds; job helper backup/restore round trip with a fake tool runner, failure paths and digest mismatches; selective preview conflicts and approval digest; schedule parser, next-run, DST and missed-run policies; retention protections; verification evaluation.

Not verified:

- No mariabackup, mbstream, mariadb-dump, mariadb client or MariaDB server ran. Tool flags (`--stream=xbstream --galera-info --extra-lsndir`, in-place `--prepare`), whether `xtrabackup_galera_info` appears in `--extra-lsndir`, version output formats, the init-file statements (`STOP ALL SLAVES`, `RESET SLAVE ALL`, the `global_priv` update) and `--skip-log-bin`/`--wsrep-provider=none` on the target image are assumptions to check in the lab.
- File ownership of restored data (the job image must run as the server's uid) and the job image itself (`/usr/local/bin/dbcm-job`) do not exist yet; there is no `cmd` binary wiring `RunJob`.
- The manager-side wiring (approval store, catalog capture, observation of an inspection instance, creation and removal of the key and init secrets, the dedicated session network, session persistence across manager restarts) is not implemented.
- No measurement of flow control or commit stalls during a backup on a loaded cluster.
- Galera position (after lab E4, unit tests only): MariaDB 11.4.13 did not write `galera_info` to `--extra-lsndir` (E4 warning). The job now scans the plaintext xbstream on its way to the sealing writer for `mariadb_backup_galera_info`/`xtrabackup_galera_info` (chunk layout and CRC-32 checked; any unexpected chunk ends the scan without failing the backup) and records the position with `source: stream`; `source: extra-lsndir` when the file exists there. Verified only against synthetic xbstreams, not against real mariabackup output; whether 11.4 puts the file into the stream is the assumption to confirm in the next lab run.
