// SPDX-License-Identifier: AGPL-3.0-only

// Package galeraprobe reads the Galera (wsrep) status of a MariaDB node with
// a dedicated monitoring account and turns it into galera.NodeHealth.
//
// It provides
//
//   - Prober: one fresh connection per probe (github.com/go-sql-driver/mysql,
//     see docs/decisions/0008-sql-driver.md), a context deadline and a short
//     default timeout. No connection is reused across probes or nodes.
//   - Checker: the runner's Checker for the node-synced and quorum
//     preconditions of Galera plans (wait-synced, verify-donor), fail closed.
//   - ProxyStatus: an adapter to the proxy's GaleraStatusProber.
//
// Monitoring account. SHOW GLOBAL STATUS needs no privilege beyond a
// logged-in account, so USAGE is enough:
//
//	CREATE USER 'dbcm_monitor'@'<proxy-or-manager-host>' IDENTIFIED BY '<secret>' REQUIRE SSL;
//	-- no GRANT: a new account has USAGE only, which allows SHOW GLOBAL STATUS
//
// Credentials come from a CredentialSource and are never logged; errors from
// this package have the password redacted. An unparsable or incomplete
// answer yields galera.HealthUnknown, which is never treated as healthy.
//
// Not verified: nothing here has run against a real MariaDB server. The tests
// use a loopback fake that speaks enough of the MySQL protocol for the real
// driver (greeting v10, mysql_native_password, COM_QUERY text resultset, TLS
// upgrade).
package galeraprobe
