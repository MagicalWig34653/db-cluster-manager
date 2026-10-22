// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics is the manager's built-in metric store: ingestion,
// time-block segment storage with a write-ahead log, retention, rollups,
// range queries with explicit gaps, and an archive tier interface.
//
// It is intentionally small. See docs/decisions/0006-metrics-engine.md for the
// engine decision, the on-disk format and the documented limits. The package
// uses only the standard library.
package metrics
