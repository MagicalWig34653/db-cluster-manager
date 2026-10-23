# ADR 0008: MySQL driver for Galera status probes

Date: 2026-10-22
Status: Proposed. Used by `internal/galeraprobe` (proxy sync probe and runner Checker). Reopen if the evidence changes or a security review rejects it.

## Context

Two components must read `wsrep_*` status from a MariaDB node: the proxy (sync state for routing) and the runner (wait-synced, verify-donor and quorum preconditions). Both need MySQL authentication, optional TLS and a text-protocol query. Neither had a client; the proxy stub returned `ErrNotImplemented` and the runner had no live-state Checker.

## Decision

Use **`github.com/go-sql-driver/mysql` v1.10.1**, pinned exactly in `go.mod`, only from inside `internal/galeraprobe`. No driver type appears in the package's exported API. Its only new transitive module is `filippo.io/edwards25519` v1.2.0 (BSD-3-Clause; required by the driver's go.mod).

## Evidence

Module proxy (`go list -m -json ...@latest`): v1.10.1, published 2026-09-02, `go 1.24.0`. `LICENSE` is Mozilla Public License 2.0 (file-level copyleft: modifications to the driver's own files must be shared; MPL-2.0 is designed to be combinable with GPL-family licenses when the driver's files are used unmodified and its notices are kept. Not legal advice, not checked by counsel). `edwards25519` is BSD-3-Clause.

Integration result: the real driver connects to a loopback fake server (greeting v10, `mysql_native_password`, TLS upgrade, text resultset) in `internal/galeraprobe` tests. **No real MariaDB was tested.**

## Alternatives

**Own minimal MySQL client.** About 400 lines for greeting, native-password scramble, TLS upgrade (SSLRequest), COM_QUERY and text resultset parsing. No new module. Rejected for now: authentication plugins (`caching_sha2_password`, `ed25519`, `mysql_clear_password` over TLS), auth switch, capability negotiation and error decoding are security-sensitive and easy to get subtly wrong against real servers we cannot yet test; the driver is widely deployed against MariaDB. The test fake is written from the protocol documentation and would only confirm our own misreadings.

**Keep the dependency (chosen).** Cost: one MPL-2.0 module plus one BSD-3-Clause module in the proxy and manager binaries. Mitigation: exact pin, driver behind `galeraprobe`, so replacing it with an own client stays local.

**`go-sqlmock`.** Rejected: it replaces the driver, so it cannot test handshake, auth or TLS behaviour.

## Probe design

- One fresh connection per probe (`sql.OpenDB` with one connection, closed afterwards); no reuse across probes or nodes; context deadline plus a short default timeout (2 s).
- `allowFallbackToPlaintext`, cleartext and old passwords are off. With TLS configured, a server that does not offer TLS fails the probe (`ErrTLSUnavailable`); there is no downgrade.
- Credentials come from a `CredentialSource` (file re-read per probe, environment variable, or static for tests), never from inline JSON. `Credentials` formats as `user:***`; errors have the password redacted and do not wrap driver errors.
- Anything incomplete, unparsable or failing yields `galera.HealthUnknown`, which is never healthy; the proxy keeps such a node sync-unknown (fail closed).

## Monitoring account

`SHOW GLOBAL STATUS` needs no privilege beyond a logged-in account, so the account has USAGE only:

```sql
CREATE USER 'dbcm_monitor'@'<proxy-or-manager-host>' IDENTIFIED BY '<secret>' REQUIRE SSL;
```

No `GRANT` is issued. `REQUIRE SSL` matches `"tls": "required"` in the proxy configuration. Unverified: the exact behaviour with MariaDB 10.11/11.x (account creation, `max_connect_errors` with the probe rate, and `SHOW GLOBAL STATUS` visibility without grants).

## Evidence (lab VMs, E5, 2026-10-23)

go-sql-driver/mysql v1.10.1 worked against real MariaDB 11.4.13 nodes in two roles ([E5/E6 results](../../tests/p0/results/e5-e6-proxy-outage.md)): inside `internal/galeraprobe` as the proxy's wsrep status source (every node Synced/Primary recognised; a killed node unusable within one probe round; a donor recognised as `donor`), and as the lab client through the proxy's TLS-terminating endpoint (verified certificate, refusal without TLS, refusal of a wrong server name). Other connectors and authentication plugins were not tested.
