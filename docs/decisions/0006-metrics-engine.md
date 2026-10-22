# ADR 0006: Metrics engine, segment format and archive tier

Date: 2026-10-22
Status: Proposed. Implemented in `internal/metrics` (U03, U04, U05). Reopen if the measured limits below are hit or the replicated-manager design needs shared metric storage.

## Context

[KONZEPT](../KONZEPT.md) requires built-in metric collection and dashboards without Grafana (U03, U04) and default retention of 7 days detailed plus 90 days aggregated, configurable and bounded by a storage limit, with an optional S3/SFTP long-term archive that dashboards query on demand with local caching (U05). [ARCHITEKTUR](../ARCHITEKTUR.md) ("Monitoring and permissions") leaves the engine, the archive format, the index and the aggregation semantics open and asks for time-range indexing, bounded download/cache space, and visible missing or unavailable intervals. Dashboard sharing must not grant access to the underlying resources.

The workload is small and known: one manager (or one active collector) ingests host, database, replication, backup and query statistics of a handful of clusters. A realistic upper bound is 10^4 to 10^5 active series at a 15 s scrape, which is 0.7k to 7k samples per second.

## Alternatives

Primary-source facts were taken from the Go module proxy (`go list -m -json`, `go mod download`, `go list -deps`, `go build`) in a scratch module on go1.27.1 darwin/arm64 on 2026-10-22. Anything I could not check is marked as such.

| Option | Findings | Assessment |
| --- | --- | --- |
| Embed the Prometheus TSDB (`github.com/prometheus/prometheus/tsdb`) | v0.315.0, published 2026-09-25. `LICENSE` is Apache-2.0. Module source is 32 MB on disk, `tsdb/` alone is about 39,600 non-test lines. A scratch `main` that only imports `tsdb` pulls in 79 external modules (697 packages in the build) and builds to a 22.3 MB binary, against 1.8 MB for an empty `main` on the same toolchain. The module is versioned 0.x and the TSDB package API is an internal-use API of the Prometheus server | Mature engine with compaction, WAL and out-of-order support, but it brings a large dependency graph (supply chain and review cost) for a workload that needs a small fraction of it. It has no archive tier, no rollups and no per-query authorizer; those would still be our code. License is compatible with AGPL-3.0-only (Apache-2.0 with GPLv3-family licenses). Not legal advice |
| VictoriaMetrics | v1.154.1, published 2026-10-19. Its `go.mod` requires `go >= 1.27.2`, which is newer than the go1.27.1 toolchain pinned in this repository, so `go mod download` refused it. The proxy did not serve the module zip in my attempt, so I could not read its `LICENSE` from the module itself (I recall Apache-2.0 for the open-source edition; not verified here). It is built and shipped as a server, not as a library | Would be a separate process to deploy, secure, upgrade and back up inside the Swarm, which contradicts "manage the monitoring infrastructure automatically" with a single manager binary. Rejected without a deeper evaluation |
| SQLite (`modernc.org/sqlite`, CGo-free) | v1.60.1, published 2026-09-29. `LICENSE` is BSD-3-Clause style ("Copyright (c) 2017 The Sqlite Authors"). The module is 151 MB on disk (transpiled C libraries). Per-row storage of `(series, ts, value)` costs 20 to 30 bytes per sample without custom blob chunking | Simple and robust, but 10 to 20 times larger than Gorilla-style chunks per sample, deletes of old data are row deletes (no whole-block drop, vacuum needed) and the large dependency has the same review cost as the Prometheus option. SQLite remains a good fit for the manager metadata, not for time series |
| Own segment files (chosen) | Standard library only. About 3049 lines of non-test code (4,600 including tests) for the engine below | Full control over retention-by-block, the archive unit, gap semantics and the authorizer hook. Cost: we own correctness, recovery and performance, and we accept the limits listed below |

## Decision

Implement a small, single-node, standard-library-only engine in `internal/metrics`:

- Series are identified by the sorted label set including `__name__`. Samples are `(timestamp ms, float64)`.
- Ingest goes into an in-memory head backed by a write-ahead log (`head.wal`). Every record is framed `u32 length, u32 CRC32C, payload`. Recovery replays until the first short, oversized or corrupt record, truncates the file there and continues appending. Syncing on each batch is on by default (`SyncWAL`).
- When the newest sample moves past the end of a block window plus the out-of-order window, the window is sealed into two immutable files, `blocks/<minT>.raw` and `blocks/<minT>.agg` (default window 2 h), and the WAL is atomically rewritten with the remaining head. A crash between the two steps is safe: replay skips samples older than the newest sealed window, and a raw-only block gets its rollup file rebuilt on open.
- Block file layout: magic, one blob per series, an index (labels, offset, length, CRC32C, sample count, min/max time), a 40-byte footer with the index CRC and the block window. Chunk reads verify the CRC; a failing chunk makes only that block tier unavailable.
- Raw chunks use Gorilla-style encoding: delta-of-delta timestamps with prefix-coded size classes (escape to 64 bit) and XOR-compressed floats with a leading/trailing-zero window (NaN payloads and negative zero are preserved bit-exactly; round-trip tested with random data).
- Rollups (1 m, 15 m, 1 h by default; all must divide the block duration) are computed at seal time and stored per series as columns `count, sum, min, max, last, inc`, where `inc` is the counter increase with reset handling.
- Retention drops whole files only: raw files older than `RawRetention` (7 d), whole blocks older than `AggRetention` (90 d), then the oldest blocks while blocks plus WAL exceed `MaxBytes`. The highest dropped time is persisted (`meta.json`) so that queries can tell "expired" from "never had data". `Append` is rejected with `ErrDiskBudget` when the budget cannot be restored.
- Ingest guards: out-of-order window (default 5 min, inserted in order) and rejection of older samples, rejection of far-future samples, conflicting duplicates rejected, idempotent duplicates ignored, limits on active series, distinct values per label, labels per series, name/value lengths, batch size and head samples. All are explicit, wrapped errors usable with `errors.Is`.
- Query: selector `=`, `!=`, `=~`, `!~` (anchored RE2), functions `last, avg, min, max, sum, count, rate, quantile` over the window `(t-window, t]`, steps aligned down to a multiple of the step. Raw samples are used wherever present; rollups serve ranges whose raw data has expired (or when `Resolution` is forced) using the largest configured resolution not above the window. Every step carries a state: `ok`, `no_data` (readable, empty) or `unavailable` (expired by retention, corrupt, archive unreachable or too large, or the function cannot be answered from rollups, for example `quantile`). The result also lists merged `Gaps` with a reason.
- `Authorizer` is mandatory and is evaluated on the label set of each candidate series (head series, block index entries) before any sample or chunk of that series is read. Dashboards are plain data (`Dashboard`, `Panel`) with validation; `Shared` only controls who can see the definition; `PanelQuery` requires the viewer's own authorizer.
- Archive tier: interface `Archive{PutBlock, GetBlock, ListRanges}`; the archive unit is a block's rollup file with the window in its id. `LocalArchive` is a directory implementation (the id is validated, writes are atomic). `ArchiveIndex` is a time-sorted in-memory index, refreshed from `ListRanges`, so a query fetches only blocks overlapping its range. Fetched blocks go through a size-bounded LRU cache; blocks larger than the cache or above `MaxArchiveBlockBytes` are not cached or fetched and show up as gaps. S3 and SFTP implementations belong to the backup backends.
- Self-protection: batch limit, head sample cap, series cap, per-query timeout, per-query sample budget (checked before decoding a chunk), step limit, concurrency limit (`ErrBusy`, no queueing), bounded archive cache, and `Stats()` with ingest, rejection, query, WAL-truncation, disk and cache counters.

## Measurements (measured, not claimed)

Machine: Apple M4 Pro (10 performance + 4 efficiency cores, 14 logical CPUs, 48 GiB RAM), macOS 27.0.1, go1.27.1 darwin/arm64, local SSD, run on 2026-10-22 with `go test -run xxx -bench . -benchtime=2s -benchmem ./internal/metrics`. Single run, no pinning, other processes active; treat as order of magnitude.

| Benchmark | Result | Meaning |
| --- | --- | --- |
| `BenchmarkAppendNoSync` | 707 ns/sample, 4 allocs/op, about 1.4 million samples/s | Batches of 1,000 series, one sample each, WAL written but not fsynced, with block sealing included |
| `BenchmarkAppendFsync` | 5.9 us/sample, about 5.9 ms per 1,000-sample batch | The same with an fsync per batch (default setting) |
| `BenchmarkQueryRaw` | 19.6 ms/query, 73 MB allocated | 100 series, 24 h at 15 s (576,000 samples decoded), avg over 5 min windows at 5 min steps |
| `BenchmarkQueryRollup` | 2.9 ms/query, 5.9 MB allocated | Same series and range forced onto the 15 min rollup |
| `BenchmarkEncodeDecodeRegular` | 21 us per 1,000 samples encode plus decode, 1.66 bytes/sample | Regular 15 s timestamps with a linearly growing value; random data compresses worse (the tests round-trip random data, they do not assert a ratio for it) |

The default ingest limits (10,000 samples per batch, 100,000 series) are far below the measured throughput.

## Limits and non-goals (documented behavior)

- Single node. A replicated manager set needs one collector or leader-only collection; metric replication is not designed here.
- The out-of-order window is store-wide: one producer with a timestamp far ahead of the others makes the others' older samples "too old". The future-skew check bounds that damage.
- All local block indexes (labels per series per block) stay in memory, and series selection scans them (no inverted index). Blocks are never compacted: 90 days at 2 h is about 1,080 blocks, each with a raw and a rollup file, which is acceptable for 10^4 series but should be revisited above that.
- Rollup answers are approximate at bucket edges: a bucket is used only if it lies fully inside the window; `rate` from rollups divides by the window length and misses the step across a block boundary. `quantile` needs raw samples. `rate` on raw data divides by the span between first and last sample in the window, without extrapolation.
- No cross-series aggregation (sum by label), no histograms, no staleness markers, no label-name or label-value queries yet.
- Only the rollup file is archived. Uploading is explicit (`ArchiveBlocks`, `Maintain`); retention does not wait for the upload, so the scheduler must run it more often than the aggregate retention. The archive is not pruned by this package.
- A WAL write error after samples entered memory returns an error but leaves the samples queryable and not durable.
- Disk budget covers blocks plus the WAL, not the archive cache (memory only) or temporary files.
- Cardinality limits count active (head) series, not all series ever seen.

## Consequences

- No new module dependencies; `go.mod` is unchanged.
- The package exposes a stable data model, `Query`/`Result` types and an `Archive` interface that a collector, the HTTP API and backup backends can build on without depending on the engine internals.
- If the limits above are hit, the intended escape hatch is to put the same `Query`/`Archive` types in front of the Prometheus TSDB; the choice is reversible because no on-disk format is exposed to users.
