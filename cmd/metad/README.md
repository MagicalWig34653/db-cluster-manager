# metad: metastore test harness

`metad` runs one node of the replicated manager metadata store (`internal/metastore`) as a standalone process. It exists for loopback tests and lab VM experiments on manager metadata HA ([ADR 0003](../../docs/decisions/0003-manager-metadata-ha-and-fencing.md)).

**This is a test harness, not product API.** It has no authentication, no authorization and no TLS, neither on the HTTP endpoint nor on the Raft transport. Run it only on loopback or an isolated lab network. It is not part of the manager distribution; `make build` builds it into `bin/metad` only because it builds every command under `cmd/`.

## Flags

| Flag | Meaning |
| --- | --- |
| `-id` | Node ID (required) |
| `-bind` | Raft TCP bind address `host:port` (required) |
| `-advertise` | Raft address peers dial; defaults to `-bind`, required when binding `0.0.0.0` |
| `-data` | Data directory: `raft.db` (bbolt log and stable store) and `snapshots/` (required) |
| `-peers` | Initial members `id=host:port,...`, including this node |
| `-bootstrap` | Bootstrap from `-peers` if the data directory has no Raft state; ignored on restart |
| `-http` | Harness HTTP listen address; must be loopback (required) |
| `-unsafe-http-any` | Allow a non-loopback `-http` address (still unauthenticated) |

All nodes of a new cluster get the same `-peers` and `-bootstrap`. A restarted node must keep its `-id`, `-bind`/`-advertise` and `-data`: the member addresses are stored in the Raft log.

## HTTP endpoints

- `GET /state` returns `{"id", "leader", "leader_id", "leader_address", "term", "barrier", "unfinished"}`. `unfinished` is the local journal's `Unfinished()` list, which may lag on followers. `GET /state?barrier=1` runs a Raft barrier first, so on the leader the list contains every committed entry; on a follower it returns 503.
- `POST /apply` takes a JSON `metastore.Command`, for example `{"Kind":"begin","OpID":"op1","OpKind":"failover","Steps":["a","b"],"Term":3}`. `Term` is the fencing term the caller holds; take it from the leader's `/state`. Responses: `200 {"ok":true,"term":N}`; `409` with code `not_owner`, `exists`, `not_found` or `rejected` for journal rejections; `503 not_leader` with the known leader's ID and Raft address; `504 outcome_unknown` when the command might still commit; `400 bad_request` for malformed or unknown JSON fields.

## Example (three nodes on loopback)

```sh
go build -o bin/metad ./cmd/metad
P=m1=127.0.0.1:7001,m2=127.0.0.1:7002,m3=127.0.0.1:7003
for i in 1 2 3; do
  bin/metad -id m$i -bind 127.0.0.1:700$i -data .local/metad/m$i \
    -http 127.0.0.1:810$i -peers $P -bootstrap &
done
curl -s 127.0.0.1:8101/state
```

SIGINT or SIGTERM stops the node cleanly. Restarting with the same flags resumes from the data directory.

## Tests

`go test ./cmd/metad/` covers flag parsing, the HTTP mapping on a single node and `TestThreeProcessesRestartFromDisk`, which builds the binary, runs three processes on loopback, stops them with SIGTERM, restarts them and checks the journal and term. `-short` skips the process test.
