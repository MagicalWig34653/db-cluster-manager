// SPDX-License-Identifier: AGPL-3.0-only

// Package proxy is a layer-4 TCP proxy for MariaDB/Galera that applies the
// decisions of internal/routing.
//
// What it does:
//
//   - A write endpoint forwards raw TCP to the writer chosen by
//     routing.Router.Decide; an optional read endpoint distributes new
//     connections round-robin over Decision.Readers. Without a writer (or
//     reader) new connections are refused with a MySQL ERR packet instead of
//     being guessed.
//   - Draining: when the writer changes, new connections go to the new
//     writer immediately; existing connections to the previous writer are
//     closed at the DrainPlan deadline. Nodes that leave the reader set are
//     drained the same way on the read endpoint. If a node becomes target
//     again before its deadline, the pending close is cancelled.
//   - Probing: a pluggable Prober runs every probe interval (at most one
//     probe in flight per node). Results become routing.Node values with
//     LastSeen and latency samples for the EWMA. A probe that never returns
//     leaves the node stale, and stale or never-reported nodes are unknown
//     and therefore not usable.
//   - Limits: maximum connections per listener and per client IP, an idle
//     timeout (no traffic in either direction; also bounds writes), a dial
//     timeout and a handshake timeout. Each connection uses two goroutines
//     and two fixed 32 KiB buffers, so resource use is bounded by
//     max_connections.
//   - Optional client-facing TLS termination inside the MySQL protocol
//     (SSLRequest upgrade, see mysqlTLSUpgrade) with certificates from a
//     CertSource that is consulted per handshake (hot reload).
//   - A loopback-only, unauthenticated GET /status JSON endpoint.
//
// Not implemented / unverified:
//
//   - The Galera status probe (MySQLGaleraStatusProber) is a stub returning
//     ErrNotImplemented: it would need monitoring credentials and MySQL
//     authentication. With "sync": "galera" every node stays sync-unknown
//     and nothing is routed (fail closed). "sync": "assume-synced" is the
//     explicit alternative for labs and tests.
//   - Nothing here has been tested against a real MariaDB server or real
//     client connectors; the protocol handling is tested against fakes
//     written from the protocol documentation.
//   - TLS termination ends TLS at the proxy; the proxy-to-node link is
//     plaintext (N06: endpoint-to-node encryption is optional and not
//     implemented). Authentication plugins that send cleartext passwords
//     because the client sees TLS (mysql_clear_password, PAM) therefore
//     send them unencrypted to the node. The auth relay supports strict
//     request/response plugins (mysql_native_password, ed25519, auth
//     switch) and caching_sha2 fast-auth; others are untested.
//   - Probes disconnect before authenticating, which MariaDB may count
//     against max_connect_errors for the proxy host (unverified).
//   - Read/write splitting correctness (transactions, read-after-write) is
//     the application's responsibility, see internal/routing.
package proxy
