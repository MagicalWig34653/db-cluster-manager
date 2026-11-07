import type { PatchHostSummary, PatchPolicy } from './api';
import type { MessageKey } from './i18n';

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

/** Mirrors the manager's policy checks so the form can name the problem before the request (the manager stays authoritative). */
export function validatePatchPolicy(p: PatchPolicy): MessageKey[] {
	const errors: MessageKey[] = [];
	const windows = p.windows ?? [];
	if (windows.length > 16) errors.push('valPatchWindowCount');
	if (windows.some((w) => !/^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(w.start))) errors.push('valPatchWindowStart');
	if (windows.some((w) => !Number.isInteger(w.durationMinutes) || w.durationMinutes < 1 || w.durationMinutes > 1440)) errors.push('valPatchWindowDuration');
	if (p.reboot && p.reboot !== 'never' && !windows.length) errors.push('valPatchRebootWindow');
	const every = p.reportEveryMinutes ?? 0;
	if (!Number.isInteger(every) || every < 0 || (every > 0 && every < 5) || every > 10080) errors.push('valPatchReportEvery');
	return errors;
}

export function validatePatchGroup(id: string, name: string): MessageKey[] {
	const errors: MessageKey[] = [];
	if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(id)) errors.push('valPatchGroupId');
	if (!name.trim() || name.length > 200) errors.push('valPatchGroupName');
	return errors;
}

/** A copy of the policy with the documented defaults filled in, so every select shows the effective value. */
export function editablePatchPolicy(p: PatchPolicy = {}): PatchPolicy {
	return { ...structuredClone(p), mode: p.mode ?? 'report_only', containerRuntime: p.containerRuntime ?? 'report_only', reboot: p.reboot ?? 'never', windows: structuredClone(p.windows ?? []) };
}

/** The request body: empty optional fields are left out (an emptied number input reads as null). */
export function patchPolicyRequest(p: PatchPolicy): PatchPolicy {
	const { reportEveryMinutes, ...rest } = p;
	return {
		...rest,
		...(reportEveryMinutes ? { reportEveryMinutes } : {}),
		windows: (p.windows ?? []).map(({ days, zone, ...w }) => ({ ...w, ...(days?.length ? { days } : {}), ...(zone ? { zone } : {}) }))
	};
}
