// Point-in-time restore helpers for the Backups page (ADR 0031). The time is
// entered and shown in UTC: the binary log events carry UTC epoch seconds, and
// the manager compares them with the requested second.

import type { PointInTimeProposal } from './api';

/**
 * Converts the value of an <input type="datetime-local"> ("2026-10-22T01:30"
 * or with seconds), read as UTC, to an RFC 3339 time. Undefined for an empty,
 * malformed or non-existent time (2026-02-30).
 */
export function utcInputToRfc3339(v: string): string | undefined {
	const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})(?::(\d{2}))?$/.exec(v.trim());
	if (!m) return undefined;
	const iso = `${m[1]}T${m[2]}:${m[3] ?? '00'}Z`;
	const d = new Date(iso);
	return Number.isNaN(d.getTime()) || d.toISOString().slice(0, 19) + 'Z' !== iso ? undefined : iso;
}

/** "2026-10-22T01:30:00Z" -> "2026-10-22 01:30:00 UTC". */
export function formatUtc(iso: string): string {
	return iso.replace('T', ' ').replace(/(\.\d+)?Z$/, ' UTC');
}

/** The restore may start only for the exact time the proposal was computed for, and only when it is covered. */
export function pitrReady(at: string | undefined, proposal: PointInTimeProposal | undefined): boolean {
	return at !== undefined && proposal !== undefined && proposal.covered && Date.parse(proposal.at) === Date.parse(at);
}
