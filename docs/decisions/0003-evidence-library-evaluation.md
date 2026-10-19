# ADR 0003 evidence: consensus library evaluation

Date: 2026-10-19
Status: Evidence for the proposed [ADR 0003](0003-manager-metadata-ha-and-fencing.md). W03 Task 1. Nothing here is a final decision; the recommendation is for the W03 prototype.

## Recommendation

Use **`github.com/hashicorp/raft@v1.8.0`** for the W03 prototype, with `github.com/hashicorp/raft-boltdb/v2@v2.4.2` as the persistent log/stable store and the library's in-memory stores and in-memory transport for tests.

Reasons, all from the evidence below:

- It ships what the prototype needs and `go.etcd.io/raft/v3` does not: a transport (including an in-process one), log/stable/snapshot stores, bootstrap and membership calls, and a leadership channel plus observer API.
- The leader term is a direct accessor: `(*Raft).CurrentTerm() uint64`.
- It starts a single node with one call and works unchanged with a one-member group (P07 single instance).
- Cost: MPL-2.0 (file-level copyleft, see licensing note), about 9 transitive modules for the core package, and a semi-opinionated design (FSM interface, own timers) with less control than etcd raft.

`go.etcd.io/raft/v3@v3.7.0` is the fallback if the prototype shows that we need control over storage, message flow or fault injection that hashicorp/raft cannot give. It is a state-machine library only: transport, persistence, snapshots and the apply loop would be our own code, which is the "own storage, snapshotting and membership code" cost listed in ADR 0003.

The SQL-replication candidate (rqlite) is not recommended as a library (see table).

## How this was gathered

Primary sources only: `go list -m -versions` and `go list -m -json` (module proxy metadata: version and publish time), the `LICENSE` file in each downloaded module, `go doc`, `go mod graph`, `go list -deps`, and the module source in the module cache. Scratch modules were created under `$TMPDIR`; nothing was added to this repository's `go.mod`. Toolchain: go1.27.1 darwin/arm64.

## Comparison

| | hashicorp/raft | etcd raft | rqlite (SQL candidate) |
| --- | --- | --- | --- |
| Module | `github.com/hashicorp/raft` | `go.etcd.io/raft/v3` | `github.com/rqlite/rqlite/v10` |
| Latest stable seen | v1.8.0, 2026-09-16 | v3.7.0, 2026-06-03 | v10.6.2, 2026-10-18 |
| License (LICENSE file) | MPL-2.0 | Apache-2.0 | MIT |
| AGPL-3.0-only compatibility | Combinable: MPL-2.0 section 3.3 permits combining with GPL-family works as a "Secondary License"; MPL files keep MPL terms. Not legal advice; not verified by counsel | Compatible (Apache-2.0 with GPLv3/AGPLv3) | Compatible (MIT) |
| go.mod `require` lines of the module (`go mod graph`) | 15 | 6 (protobuf, plus test-only datadriven, testify, go-spew, go-difflib, yaml) | 86 |
| External modules in the build of the core package (`go list -deps`) | 9: go-hclog, go-immutable-radix, go-metrics, go-msgpack/v2, golang-lru, fatih/color, mattn/go-colorable, mattn/go-isatty, x/sys | 1: google.golang.org/protobuf | not measured (86 direct requires; uses a forked cgo `go-sqlite3` via `replace`) |
| With raft-boltdb/v2 v2.4.2 | 13 external modules for raft + raft-boltdb/v2 (adds bbolt, x/sys and others) | n/a | n/a |
| Leader term exposed | Yes: `CurrentTerm() uint64`; also `Stats()["term"]` (string) | Yes: `Node.Status().GetTerm()` (and `HardState.Term` in `Ready`) | Internal to its store; not a documented embedding API |
| In-process test transport | Yes: `NewInmemTransport(addr)` (single node run below; multi-node `Connect` not exercised) | None. The application moves `Ready.Messages` itself, so an in-memory network is a few dozen lines but is our code | Not applicable (HTTP and TCP server) |
| Persistence | `raft-boltdb/v2` (bbolt) or `InmemStore`; file snapshot store in the library | `MemoryStorage` only; the durable WAL/snapshot code lives in the etcd server, not this module | SQLite plus Raft log managed by `rqlited` |
| Leadership observation | `LeaderCh()`, `RegisterObserver`, `State()`, `Leader()`, `LeadershipTransfer` | `Ready.SoftState`, `Status()` | HTTP status endpoints |
| Embeddable as a library | Yes (primary use) | Yes (primary use) | Not documented as a library; it is a database server with its own binaries (`cmd/rqlited`) |
| Fit to ADR 0003 | Good | Good but most build work | Poor: second SQL service, would need cgo SQLite, fencing term not exposed |

The other SQL candidate looked at, `github.com/canonical/go-dqlite/v3@v3.0.4` (Apache-2.0, 2026-02-18), needs the C library `libdqlite` per its README (`apt install libdqlite-dev`). That would break a pure-Go single-binary build and is rejected without further trial. A build of its `app` package was started but stopped at a missing `go.sum` entry, so "needs C library" rests on the README only.

## Experiment: single node, become leader, apply one entry, print the term

Both programs live only in the scratch directory `$TMPDIR/tmp.b8y7bzaQ2m/{hc,etcd}` (hc 37 lines, etcd 48 lines) and are not in the repository. Each passed `go vet`.

hashicorp/raft v1.8.0 (in-memory log, stable and snapshot stores, in-memory transport, bootstrap with one server, `LeaderCh`, `Apply`, `CurrentTerm`, `Shutdown`):

```
leader: true
apply err: <nil> resp: hello index: 3
state: Leader term: 2 (Stats: 2 )
shutdown: <nil>
```

The `index: 3` is the entry index after the bootstrap configuration entry and the leader's no-op entry; I did not inspect which log entries occupy indexes 1 and 2.

etcd raft v3.7.0 (`MemoryStorage`, `StartNode` with one peer, ticker, `Ready` loop, `Propose`, `Status().GetTerm()`, `Stop`; its INFO log lines omitted):

```
leader, proposing
applied "hello" index=3
state: StateLeader term: 2
stopped
```

Observations while writing them: hashicorp/raft took one constructor call and a bootstrap call; etcd raft required the application to run the ticker, persist `Ready` output, apply committed entries and call `Advance`. The v3.7.0 API uses protobuf pointer types (`[]*pb.Entry`, `*pb.HardState`, getters), which my first draft got wrong and had to be fixed. My etcd program does not follow the "persist before sending messages" contract (single node, in-memory only).

## Minimal API surface needed by Task 4 (hashicorp/raft)

| Need | API |
| --- | --- |
| Start node with in-memory/loopback transport | `raft.NewRaft(cfg, fsm, logStore, stableStore, snapStore, transport)`, `raft.NewInmemTransport`, `raft.NewInmemStore`, `raft.NewInmemSnapshotStore`, `BootstrapCluster` |
| Apply an entry on the leader | `Apply(cmd []byte, timeout)` returning an `ApplyFuture` with `Error()`, `Response()`, `Index()` |
| Current term | `CurrentTerm()` |
| Observe leadership changes | `LeaderCh()` (bool channel), `RegisterObserver` with `LeaderObservation`, `State()` |
| Shut down | `Shutdown().Error()` |

The caller must implement the `FSM` interface (`Apply`, `Snapshot`, `Restore`).

## Not verified

- Multi-node behavior of either library: election with three nodes, partitions, leader loss, term changes after failover, fencing semantics. This is exactly W03's later work.
- `CurrentTerm()` stability for fencing: that a term only grows across restart with `raft-boltdb` persistence was not tested; only in-memory single-node runs were done.
- `raft-boltdb/v2` was added to the dependency graph and counted but never run.
- Whether an in-memory network between several hashicorp nodes (`InmemTransport.Connect`) supports injected partitions as needed; not exercised.
- Memory and CPU use against the budget in [TESTING.md](../TESTING.md).
- MPL-2.0 versus AGPL-3.0-only: read from the license text and general knowledge only; no legal review. Transitive licenses of the dependencies were not enumerated.
- rqlite and go-dqlite were not run. rqlite's dependency count is a `go mod graph` line count only; its build was not attempted. The cgo requirement for go-dqlite is from its README; the cgo status of rqlite's SQLite fork is inferred from the `replace` directive in its `go.mod`.
- Dates are module proxy publish times, not release-note dates; release notes were not read.
- No search for known security advisories of any candidate was done.
