// SPDX-License-Identifier: AGPL-3.0-only

// Package notify implements the event model, the bounded operation/audit
// event log, notification routing (rules, deduplication, cooldown, flapping
// suppression, escalation) and delivery channels (UI inbox, webhook, email).
//
// Scope and limits: the Log is in-memory only and bounded. Durable storage,
// retention policy and export (KONZEPT U07) are out of scope and must be
// layered on top of Log.Snapshot/Restore. Events must never carry secrets;
// callers are responsible for redacting Details before appending.
//
// Observations of unknown state use SeverityUnknown, which is the zero value
// and is deliberately treated as at least warning when filtering, so unknown
// never looks healthy.
package notify
