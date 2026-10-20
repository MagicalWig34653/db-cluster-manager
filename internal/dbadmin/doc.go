// SPDX-License-Identifier: AGPL-3.0-only

// Package dbadmin implements database, user and permission management for
// MariaDB (requirements D01, D02, D04).
//
// All SQL is generated here and executed through the Executor interface, so
// the package never talks to a database driver itself. Operations are split
// into planning (Admin.Plan*, which may issue read-only queries but never
// mutates anything) and execution (Admin.Apply).
//
// Galera notes, enforced in code:
//   - Mutating operations run on the current writer only. Plan records the
//     writer node, and Apply re-checks it and refuses to run elsewhere.
//   - DDL and account statements replicate via Total Order Isolation (TOI)
//     and briefly block other DDL cluster-wide. Every Plan carries warnings.
//   - Statements use IF [NOT] EXISTS wherever MariaDB supports it so that a
//     partially applied Plan can be applied again.
//
// Identifiers are validated against strict ASCII allowlists before they are
// quoted; anything else is rejected rather than escaped. Values that MariaDB
// cannot bind as parameters in DDL (account passwords) are restricted to a
// character set that needs no escaping, which also makes the result
// independent of sql_mode (NO_BACKSLASH_ESCAPES).
package dbadmin
