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
//   - The Galera status source is injected (Options.Status); this package
//     has no MySQL client or credential handling. cmd/proxy wires
//     internal/galeraprobe (monitoring account, TLS, per-probe connection).
//     Without a source, "sync": "galera" leaves every node sync-unknown and
//     nothing is routed (fail closed). "sync": "assume-synced" is the
//     explicit alternative for labs and tests.
//   - Lab E5 (tests/p0/results/e5-e6-proxy-outage.md) ran cmd/proxy in
//     front of a real 3-node MariaDB 11.4 Galera cluster on arm64 VMs with
//     go-sql-driver/mysql and the MariaDB 11.4 CLI: writer selection,
//     failover after a killed writer, hysteresis, reload-driven drain,
//     round-robin reads and TLS termination worked. Other connectors,
//     authentication plugins other than mysql_native_password, several
//     proxy instances and VIPs are untested.
//   - A writer that becomes Donor (it serves IST/SST to a returning node)
//     is not usable, so one node failure can cause two writer changes
//     (observed in E5).
//   - TLS termination ends TLS at the proxy; the proxy-to-node link is
//     plaintext (N06: endpoint-to-node encryption is optional and not
//     implemented). Authentication plugins that send cleartext passwords
//     because the client sees TLS (mysql_clear_password, PAM) therefore
//     send them unencrypted to the node. The auth relay supports strict
//     request/response plugins (mysql_native_password, ed25519, auth
//     switch) and caching_sha2 fast-auth; others are untested.
//   - Probes disconnect before authenticating: every probe is an aborted
//     connect on the node (E5: about two per second and node,
//     Aborted_connects_preauth 750 after 6 minutes). MariaDB blocks a host
//     after max_connect_errors only while name resolution keeps a host cache;
//     the Galera service spec sets skip_name_resolve explicitly.
//   - Read/write splitting correctness (transactions, read-after-write) is
//     the application's responsibility, see internal/routing.
package proxy
