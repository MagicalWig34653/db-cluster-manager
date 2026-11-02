import type { PatchHostSummary } from './api';

export type PatchAttention = 'error' | 'stale' | 'security' | 'reboot' | 'updates' | 'unknown' | 'ok';

const order: PatchAttention[] = ['error', 'stale', 'security', 'reboot', 'updates', 'unknown', 'ok'];

/** The most urgent reason to look at a host; a host is only "ok" when its report is fresh and complete. */
export function hostAttention(h: PatchHostSummary): PatchAttention {
	if (h.host.lastPollError) return 'error';
	if (h.stale) return 'stale';
	if (h.securityPending > 0) return 'security';
	if (h.rebootRequired === 'yes') return 'reboot';
	if (h.updatesPending > 0) return 'updates';
	if (!h.securityKnown || h.rebootRequired === 'unknown') return 'unknown';
	return 'ok';
}

export function sortPatchHosts(list: PatchHostSummary[]): PatchHostSummary[] {
	return [...list].sort((a, b) => order.indexOf(hostAttention(a)) - order.indexOf(hostAttention(b)) || b.securityPending - a.securityPending || a.host.name.localeCompare(b.host.name));
}
