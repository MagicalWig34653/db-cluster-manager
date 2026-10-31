// SPDX-License-Identifier: AGPL-3.0-only

// Package dbbackup plans and executes backups and restores of MariaDB Galera
// cluster data (requirements B01-B06, release gate R1 "verified full
// restore"; method decision in ADR 0009).
//
// The package has three layers:
//
//   - Planning (pure): source node selection (SelectSource), BackupPlan,
//     ClusterRestorePlan (full restore into a NEW cluster),
//     InspectionRestorePlan (isolated single-node instance with wsrep off) and
//     its TeardownPlan, and the selective restore preview and plan. Plans are
//     runner.Operation values whose command steps use agent command kinds
//     (run-backup, run-restore, remove-job, create-volume, init-identity,
//     create-service, ...). Gates carry conditions that Checker evaluates.
//   - Job helper (RunJob): the code that runs inside the owned one-shot job
//     service the agent launches. It invokes only allow-listed tools
//     (mariabackup, mbstream, mariadb-dump, mariadb), encrypts the stream
//     with backup.SealWriter for the recovery key's public half, streams it
//     to the backend with backup.PutStream, and writes the integrity manifest
//     last, so its presence marks a complete backup.
//   - Policy: verification evaluation and the last-verified record, retention
//     on top of backup.Select, and the schedule model.
//
// Safety rules enforced here (and again by the agent where it can):
//
//   - A restore never targets the source cluster's identity, a production
//     cluster ID, name, network or volume. Physical restores always get a new
//     cluster identity; the restore job deletes Galera state files so the
//     restored node bootstraps a new history.
//   - Inspection instances run with wsrep off, without a provider, without a
//     cluster address, on a separate network, with replication not started,
//     production accounts locked and a dedicated inspection account. They
//     have an expiry, resource limits and a teardown plan.
//   - Every command step of a restore operation carries an approval
//     condition, and the agent refuses run-restore without an approval ID.
//
// Point-in-time recovery (ADR 0031) adds the binlog archive job
// (runArchiveJob, BinlogArchivePlan), the chain rules (binlogChain,
// ResolvePITR, CoverageOf) and the replay job (runReplayJob), which an
// inspection restore plan appends when InspectionRequest.PITR is set. A
// replay only ever targets an inspection instance.
//
// Tool invocations go through the ToolRunner port and are tested with fakes
// here; the real tools ran only in the lab experiments listed in the guides.
package dbbackup
