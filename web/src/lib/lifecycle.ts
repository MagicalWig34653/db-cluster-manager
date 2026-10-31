// Pure rules for the lifecycle flows: operation phase and progress, plan
// descriptions shown before an approval, typed delete confirmation, outage
// recovery gating, backup policy and notification settings forms. The manager
// enforces everything again; these keep the controls honest and the wording
// accurate.

import type {
	BackupPolicy,
	RestoreVerifyPolicy,
	Diagnosis,
	ManagerEvent,
	MinSeverity,
	NotificationSettings,
	NotificationSettingsInput,
	Operation,
	OperationProgress
} from './api';
import { minSeverities } from './api';
import type { MessageKey } from './i18n';
import { approvalView } from './operations';

// ---------------------------------------------------------------------------
// Operation phase and progress

export type Phase = 'awaitingApproval' | 'paused' | 'running' | 'succeeded' | 'failed' | 'aborted' | 'unknown';

/**
 * What an operation is doing, from the fields the manager reports. A paused
 * operation reports state "pending"; one with an unapproved approval is waiting
 * for that approval, not paused.
 */
export function phase(op: Pick<Operation, 'state' | 'approval'>): Phase {
	if (op.state === 'succeeded' || op.state === 'failed' || op.state === 'aborted') return op.state;
	if (op.state === 'unknown') return 'unknown';
	if (approvalView(op).kind === 'required') return 'awaitingApproval';
	return op.state === 'pending' ? 'paused' : 'running';
}

export const phaseLabel: Record<Phase, MessageKey> = {
	awaitingApproval: 'phaseAwaitingApproval',
	paused: 'phasePaused',
	running: 'phaseRunning',
	succeeded: 'opSucceeded',
	failed: 'opFailed',
	aborted: 'opAborted',
	unknown: 'stateUnknown'
};

export interface ProgressView {
	done: number;
	total: number;
	/** 0..100, rounded down; 0 when the total is 0. */
	percent: number;
	/** Current step name, if reported. */
	step?: string;
	target?: string;
	waiting?: string;
	/** Latest failed attempt of the current step (already redacted by the manager). */
	lastError?: string;
	attempts: number;
	polls: number;
	/** True when the step journal state says the step failed. */
	stepFailed: boolean;
}

/** Normalises the reported progress; implausible values (done above total) are clamped, never extended. */
export function progressView(p: OperationProgress | undefined): ProgressView | undefined {
	if (!p) return undefined;
	const total = Math.max(0, p.stepsTotal);
	const done = Math.min(Math.max(0, p.stepsDone), total);
	return {
		done,
		total,
		percent: total === 0 ? 0 : Math.floor((done * 100) / total),
		step: p.step || undefined,
		target: p.target || undefined,
		waiting: p.waiting || undefined,
		lastError: p.lastError || undefined,
		attempts: p.attempts ?? 0,
		polls: p.polls ?? 0,
		stepFailed: p.state === 'failed'
	};
}

// ---------------------------------------------------------------------------
// Plan descriptions (shown before an approval)

/**
 * The step list the manager documents for an operation kind (api/openapi.yaml).
 * The API exposes no per-step plan, so only kinds whose steps are documented
 * get a list; everything else shows the digest only.
 */
export function planSteps(kind: string): MessageKey[] | undefined {
	if (kind === 'cluster/delete') return ['planDelete1', 'planDelete2', 'planDelete3', 'planDelete4', 'planDelete5'];
	if (kind === 'galera/full-outage-recovery')
		return ['planOutage1', 'planOutage2', 'planOutage3', 'planOutage4', 'planOutage5', 'planOutage6', 'planOutage7'];
	if (kind.startsWith('inspection') || kind.startsWith('restore')) return ['planInspect1', 'planInspect2', 'planInspect3', 'planInspect4'];
	return undefined;
}

/** What survives the operation; shown next to the destructive plan. */
export function planKept(kind: string): MessageKey | undefined {
	return kind === 'cluster/delete' ? 'planDeleteKept' : undefined;
}

// ---------------------------------------------------------------------------
// Typed delete confirmation

/** The user must type the cluster name exactly; surrounding whitespace is ignored, case is not. */
export const deleteConfirmed = (typed: string, name: string): boolean => name !== '' && typed.trim() === name;

// ---------------------------------------------------------------------------
// Outage recovery

/** Nodes whose position is not known yet (no saved seqno), for which the optional position recovery helps. */
export function unknownPositions(d: Diagnosis): string[] {
	return d.positions.filter((p) => p.seqno === undefined || p.seqno < 0).map((p) => p.slot);
}

/**
 * A recovery can be requested only for the candidate the diagnosis itself
 * proposes; the manager re-checks this against fresh evidence.
 */
export const canRequestRecovery = (d: Diagnosis): boolean => !!d.candidate && d.candidate !== '';

/** Highest known sequence number, for the "why this node" summary. */
export function highestSeqno(d: Diagnosis): number | undefined {
	const known = d.positions.map((p) => p.seqno).filter((s): s is number => s !== undefined && s >= 0);
	return known.length === 0 ? undefined : Math.max(...known);
}

// ---------------------------------------------------------------------------
// Node events of a cluster

/** Events that belong to a cluster, newest first, limited. Time ties keep input order. */
export function eventsForCluster(events: readonly ManagerEvent[], cluster: string, limit = 20): ManagerEvent[] {
	return events
		.filter((e) => e.cluster === cluster)
		.map((e, i) => ({ e, i, t: Date.parse(e.time) }))
		.sort((a, b) => (Number.isNaN(b.t) ? -Infinity : b.t) - (Number.isNaN(a.t) ? -Infinity : a.t) || a.i - b.i)
		.slice(0, limit)
		.map((x) => x.e);
}

// ---------------------------------------------------------------------------
// Backup policy form

export interface PolicyForm {
	enabled: boolean;
	schedule: string;
	timezone: string;
	missed: 'skip' | 'run-once';
	method: 'physical' | 'logical';
	/** Logical method: database names, separated by commas, spaces or new lines. */
	databases: string;
	retentionEnabled: boolean;
	keepLast: string;
	keepDaily: string;
	keepWeekly: string;
	archiveEnabled: boolean;
	archiveSchedule: string;
	/** Not edited here; kept so that saving the form does not drop it. */
	verify?: RestoreVerifyPolicy;
}

export const policyFormOf = (p: BackupPolicy | undefined): PolicyForm => ({
	enabled: p?.enabled ?? false,
	schedule: p?.schedule ?? '0 2 * * *',
	timezone: p?.timezone ?? 'UTC',
	missed: p?.missed ?? 'skip',
	method: p?.method ?? 'physical',
	databases: (p?.databases ?? []).join(', '),
	retentionEnabled: p?.retention !== undefined,
	keepLast: String(p?.retention?.keepLast ?? 7),
	keepDaily: String(p?.retention?.keepDaily ?? 0),
	keepWeekly: String(p?.retention?.keepWeekly ?? 0),
	archiveEnabled: p?.archive?.enabled ?? false,
	archiveSchedule: p?.archive?.schedule ?? '0 * * * *',
	verify: p?.verify
});

const cronField = /^(\*|\d+(-\d+)?)(\/\d+)?(,(\*|\d+(-\d+)?)(\/\d+)?)*$/;

/** Shape check only (five fields, each a list of *, numbers, ranges or steps); the manager validates the values. */
export function validCronShape(spec: string): boolean {
	const f = spec.trim().split(/\s+/);
	return spec.length <= 128 && f.length === 5 && f.every((x) => cronField.test(x));
}

export function validTimezone(tz: string): boolean {
	if (tz.trim() === '') return true; // manager default UTC
	try {
		new Intl.DateTimeFormat('en', { timeZone: tz.trim() });
		return true;
	} catch {
		return false;
	}
}

const dbNameRE = /^[A-Za-z0-9_$-]{1,64}$/;
/** Database names of the logical method; duplicates are dropped, order is kept. */
export const parseDatabases = (s: string): string[] => [...new Set(s.split(/[,;\s]+/).map((x) => x.trim()).filter(Boolean))];

export type PolicyError = 'schedule' | 'timezone' | 'databases' | 'keepLast' | 'keepDaily' | 'keepWeekly' | 'archiveSchedule';

const countIn = (s: string, lo: number) => /^\d+$/.test(s.trim()) && Number(s) >= lo && Number(s) <= 1000;

export function validatePolicy(f: PolicyForm): PolicyError[] {
	const out: PolicyError[] = [];
	if (!validCronShape(f.schedule)) out.push('schedule');
	if (!validTimezone(f.timezone)) out.push('timezone');
	if (f.method === 'logical') {
		const dbs = parseDatabases(f.databases);
		if (dbs.length === 0 || dbs.length > 64 || !dbs.every((d) => dbNameRE.test(d))) out.push('databases');
	}
	if (f.archiveEnabled && !validCronShape(f.archiveSchedule)) out.push('archiveSchedule');
	if (f.retentionEnabled) {
		if (!countIn(f.keepLast, 1)) out.push('keepLast');
		if (!countIn(f.keepDaily, 0)) out.push('keepDaily');
		if (!countIn(f.keepWeekly, 0)) out.push('keepWeekly');
	}
	return out;
}

export function buildPolicy(f: PolicyForm): BackupPolicy {
	return {
		enabled: f.enabled,
		schedule: f.schedule.trim().split(/\s+/).join(' '),
		...(f.timezone.trim() ? { timezone: f.timezone.trim() } : {}),
		missed: f.missed,
		method: f.method,
		...(f.method === 'logical' ? { databases: parseDatabases(f.databases) } : {}),
		...(f.retentionEnabled
			? { retention: { keepLast: Number(f.keepLast), keepDaily: Number(f.keepDaily), keepWeekly: Number(f.keepWeekly) } }
			: {}),
		...(f.verify ? { verify: f.verify } : {}),
		...(f.archiveEnabled || f.archiveSchedule !== '0 * * * *'
			? { archive: { enabled: f.archiveEnabled, schedule: f.archiveSchedule.trim().split(/\s+/).join(' '), ...(f.timezone.trim() ? { timezone: f.timezone.trim() } : {}) } }
			: {})
	};
}

// ---------------------------------------------------------------------------
// Notification settings form

export interface NotifyForm {
	minSeverity: MinSeverity;
	webhookEnabled: boolean;
	webhookUrl: string;
	webhookSecret: string;
	allowPrivate: boolean;
	emailEnabled: boolean;
	recipients: string;
	addr: string;
	from: string;
	username: string;
	password: string;
}

export const notifyFormOf = (s: NotificationSettings): NotifyForm => ({
	minSeverity: s.minSeverity ?? 'warning',
	webhookEnabled: s.webhook.enabled,
	webhookUrl: s.webhook.url ?? '',
	webhookSecret: '',
	allowPrivate: s.webhook.allowPrivate ?? false,
	emailEnabled: s.email.enabled,
	recipients: s.email.recipients.join(', '),
	addr: s.email.addr ?? '',
	from: s.email.from ?? '',
	username: s.email.username ?? '',
	password: ''
});

export const parseRecipients = (s: string): string[] =>
	s
		.split(/[,;\s]+/)
		.map((r) => r.trim())
		.filter(Boolean);

const emailRE = /^[^\s@]+@[^\s@]+$/;
const hostPortRE = /^[A-Za-z0-9._-]+:\d{1,5}$|^\[[0-9A-Fa-f:]+\]:\d{1,5}$/;

export type NotifyError = 'webhookUrl' | 'recipients' | 'addr' | 'from';

export function validateNotify(f: NotifyForm): NotifyError[] {
	const out: NotifyError[] = [];
	if (f.webhookEnabled) {
		try {
			const u = new URL(f.webhookUrl.trim());
			if (u.protocol !== 'https:' && u.protocol !== 'http:') out.push('webhookUrl');
		} catch {
			out.push('webhookUrl');
		}
	}
	if (f.emailEnabled) {
		const r = parseRecipients(f.recipients);
		if (r.length === 0 || r.length > 50 || !r.every((x) => emailRE.test(x))) out.push('recipients');
		if (!hostPortRE.test(f.addr.trim())) out.push('addr');
		if (!emailRE.test(f.from.trim())) out.push('from');
	}
	return out;
}

/** Secrets are sent only when typed; absent fields keep the stored value on the manager. */
export function buildNotify(f: NotifyForm): NotificationSettingsInput {
	return {
		minSeverity: f.minSeverity,
		webhook: {
			enabled: f.webhookEnabled,
			url: f.webhookUrl.trim(),
			allowPrivate: f.allowPrivate,
			...(f.webhookSecret ? { secret: f.webhookSecret } : {})
		},
		email: {
			enabled: f.emailEnabled,
			recipients: parseRecipients(f.recipients),
			addr: f.addr.trim(),
			from: f.from.trim(),
			username: f.username.trim(),
			...(f.password ? { password: f.password } : {})
		}
	};
}

export const isMinSeverity = (v: string): v is MinSeverity => (minSeverities as readonly string[]).includes(v);
