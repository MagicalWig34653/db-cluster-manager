# E15: point-in-time recovery with the real job helper on lab VMs, 2026-10-31

Script `tests/p0/e15-pitr.sh` ([log](e15-pitr.log), sanitized; passwords redacted), ADR [0031](../../../docs/decisions/0031-point-in-time-recovery.md). Three Lima VMs (arm64, 2 vCPU / 4 GiB / 20 GiB, created and removed with `bin/lab`), Docker Swarm, MariaDB 11.4.13 Galera with the binary log options of `galera.BuildServiceSpec` (`binlog: true`: `--log_bin`, `--log_slave_updates=ON`, `--binlog_expire_logs_seconds`) and the product's volume layout (volume at `/dbcm`, datadir `/dbcm/data`, socket `/dbcm/run`). Host before the run: load 6, 67 GiB free. `bin/lab cleanup --apply` ran at the end (no `dbcm-lab` VM left, unrelated Lima/Colima entries untouched).

## What ran

The **real `cmd/dbcm-job`** (cross-built for linux/arm64) with the real `mariabackup`, `mbstream`, `mariadb` and `mariadb-binlog` of the mariadb image, started with `docker run` the way the agent builds the job (same argv, read-only source volume, secrets directory, dir backup target). Library calls (key generation, init file, option file, stop time, coverage) through `cmd/pitrlab`.

| Step | Result |
| --- | --- |
| Data before the backup | `pitr.orders` 2000 rows (batch O), written on galera-1 |
| Physical backup of galera-2 (job helper) | 2 s; manifest has `binlog_base` `dbcm-bin.000002:383` (see finding 1) |
| Writes after the backup | batch A (1000 inserts, updates on galera-3, deletes on galera-1), then time T (recorded on the host, UTC), then batch B (inserts on galera-3, updates on galera-1, deletes on the **source node** galera-2) |
| Binlog archive (job helper, `--tool=binlog-archive`) | 1 s; archive of the closed file(s); a second run with nothing new succeeded |
| Coverage over the real archive | T covered with 1 segment; a time after the archive not covered ("the archive ends at ..., later changes are not archived yet"); a time before the backup not covered (named) |
| Physical restore into a new volume + instance with the product's init file (wsrep off, port 3307, replay account) | restored state equals the backup-time state: 2000 rows, `CHECKSUM TABLE` 743274767 |
| Refusals | replay against a production service name refused by the job; replay beyond the archive refused before any statement was applied |
| Replay up to T (job helper, `mariadb-binlog --start-position --stop-datetime`) | 1 s |
| **Comparison** | restored to T: 2960 rows, sum(amount) 337347, sum(id) 4462460, batches A,O, `CHECKSUM TABLE` **1523331441**, identical to the cluster's state at T (read on galera-1 with `wsrep_sync_wait`); the later state (3817 rows, batches A,B,O, checksum 1379651463) differs, so the stop time worked |
| Replay account | `dbcm_replay` is gone after the replay (account list of the restored instance) |

## Findings and fixes (found by this run)

1. **MariaDB 11.4 streams the binlog position in the xbstream.** `mariadb_backup_binlog_info` is a file in the stream and not written to `--extra-lsndir` (like `galera_info`, E4). The job only looked in `--extra-lsndir`, so the first real backup had no `binlog_base` and could never be used for PITR. The stream tap now also keeps `mariadb_backup_binlog_info`/`xtrabackup_binlog_info` (test `TestInfoTapFindsBinlogInfoNextToGaleraInfo`).
2. Harness only: seq tables need a selected database; the E1 helpers use the default socket path and datadir, the lab script overrides them for the product layout. Three aborted runs before the passing one (each cleaned up with `bin/lab cleanup --apply`).

## Not run (honest limits)

- The **manager, agent and UI** were not part of this run: the jobs were started with `docker run`, and approval, the replay and teardown steps of the operation, the replay secret and the proposal/archive API are covered by fakes and in-process tests only.
- One archived file and one point in time; no archive across several files, no gap or node-replacement case on real data, no long-running or loaded cluster, no timing at production size.
- Time precision: T and the phase boundaries are at least a few seconds apart; events within the same second as T were not exercised against real MariaDB.
- GTID continuation across source nodes, scheduled archiving, and enabling the binary log on an existing cluster do not exist.
