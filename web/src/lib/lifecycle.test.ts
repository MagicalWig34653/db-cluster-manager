import { describe, expect, it } from 'vitest';
import { createClient, isOperation, isOutageRecoveryStarted, isNotificationSettings, isBackupSummary, isClusterDeletion, type Diagnosis, type ManagerEvent, type Operation } from './api';
import {
	buildNotify,
	buildPolicy,
	canRequestRecovery,
	deleteConfirmed,
	eventsForCluster,
	highestSeqno,
	notifyFormOf,
	parseRecipients,
	phase,
	planSteps,
	policyFormOf,
	progressView,
	unknownPositions,
	validateNotify,
	validatePolicy
} from './lifecycle';
import { messageKeys } from './i18n';

const digest = 'b'.repeat(64);
const op = (o: Partial<Operation> = {}): Operation => ({ id: 'o', kind: 'cluster/delete', state: 'pending', ...o });

describe('operation phase and progress', () => {
	it('separates waiting for approval from paused', () => {
		expect(phase(op({ approval: { digest } }))).toBe('awaitingApproval');
		expect(phase(op())).toBe('paused');
		expect(phase(op({ state: 'running', approval: { digest, approvedBy: 'a' } }))).toBe('running');
		expect(phase(op({ state: 'pending', approval: { digest, approvedBy: 'a' } }))).toBe('paused');
		expect(phase(op({ state: 'failed', approval: { digest } }))).toBe('failed');
		expect(phase(op({ state: 'aborted', approval: { digest } }))).toBe('aborted');
		expect(phase(op({ state: 'unknown' }))).toBe('unknown');
	});
	it('derives a clamped progress view and never invents progress', () => {
		expect(progressView(undefined)).toBeUndefined();
		expect(progressView({ stepsDone: 3, stepsTotal: 12, step: 'await-stopped', waiting: 'tasks still running', state: 'pending', polls: 4 })).toMatchObject({
			done: 3,
			total: 12,
			percent: 25,
			step: 'await-stopped',
			waiting: 'tasks still running',
			polls: 4,
			stepFailed: false
		});
		expect(progressView({ stepsDone: 1, stepsTotal: 4, lastError: 'agent unreachable' })?.lastError).toBe('agent unreachable');
		expect(progressView({ stepsDone: 1, stepsTotal: 4, lastError: '' })?.lastError).toBeUndefined();
		expect(isOperation({ id: 'o', kind: 'k', state: 'running', progress: { stepsDone: 1, stepsTotal: 4, lastError: 7 } })).toBe(false);
		expect(progressView({ stepsDone: 9, stepsTotal: 4 })).toMatchObject({ done: 4, percent: 100 });
		expect(progressView({ stepsDone: 0, stepsTotal: 0 })).toMatchObject({ percent: 0 });
		expect(progressView({ stepsDone: 1, stepsTotal: 2, state: 'failed' })?.stepFailed).toBe(true);
	});
	it('accepts and rejects progress in the operation guard', () => {
		const base = { id: 'o', kind: 'k', state: 'running' };
		expect(isOperation({ ...base, progress: { stepsDone: 1, stepsTotal: 3, step: 's', since: '2026-10-20T10:00:00Z' } })).toBe(true);
		expect(isOperation({ ...base, progress: { stepsDone: -1, stepsTotal: 3 } })).toBe(false);
		expect(isOperation({ ...base, progress: { stepsDone: 1 } })).toBe(false);
		expect(isOperation({ ...base, progress: { stepsDone: 1, stepsTotal: 3, since: 'yesterday' } })).toBe(false);
	});
	it('lists plan steps only for kinds whose steps the API documents', () => {
		expect(planSteps('cluster/delete')?.length).toBe(5);
		expect(planSteps('galera/full-outage-recovery')?.length).toBe(7);
		expect(planSteps('galera/replace')).toBeUndefined();
	});
});

describe('typed delete confirmation', () => {
	it('requires the exact cluster name', () => {
		expect(deleteConfirmed('prod-db', 'prod-db')).toBe(true);
		expect(deleteConfirmed('  prod-db ', 'prod-db')).toBe(true);
		expect(deleteConfirmed('Prod-db', 'prod-db')).toBe(false);
		expect(deleteConfirmed('prod', 'prod-db')).toBe(false);
		expect(deleteConfirmed('', '')).toBe(false);
	});
	it('guards the deletion answer and requires a 64-hex digest', () => {
		expect(isClusterDeletion({ id: 'o', kind: 'cluster/delete', approval: { digest } })).toBe(true);
		expect(isClusterDeletion({ id: 'o', kind: 'cluster/delete', approval: { digest: 'abc' } })).toBe(false);
		expect(isClusterDeletion({ id: 'o', kind: 'cluster/delete' })).toBe(false);
	});
});

describe('outage recovery', () => {
	const d = (x: Partial<Diagnosis> = {}): Diagnosis => ({
		candidate: 'n2',
		positions: [
			{ slot: 'n1', source: 'grastate', seqno: -1 },
			{ slot: 'n2', source: 'grastate', seqno: 120, safe_to_bootstrap: false },
			{ slot: 'n3', source: 'unknown' }
		],
		reasons: [],
		requires_approval: true,
		...x
	});
	it('finds nodes without a known position and the highest seqno', () => {
		expect(unknownPositions(d())).toEqual(['n1', 'n3']);
		expect(highestSeqno(d())).toBe(120);
		expect(highestSeqno(d({ positions: [] }))).toBeUndefined();
	});
	it('offers a recovery request only for a proposed candidate', () => {
		expect(canRequestRecovery(d())).toBe(true);
		expect(canRequestRecovery(d({ candidate: undefined }))).toBe(false);
	});
	it('guards the recovery answer including its diagnosis', () => {
		const ok = { id: 'o', kind: 'galera/full-outage-recovery', candidate: 'n2', approval: { digest }, diagnosis: d() };
		expect(isOutageRecoveryStarted(ok)).toBe(true);
		expect(isOutageRecoveryStarted({ ...ok, diagnosis: { positions: 'x' } })).toBe(false);
		expect(isOutageRecoveryStarted({ ...ok, approval: {} })).toBe(false);
	});
});

describe('node events', () => {
	const ev = (id: string, cluster: string | undefined, time: string): ManagerEvent => ({ id, cluster, time, severity: 'warning', message: id });
	it('selects the cluster events newest first and limits them', () => {
		const events = [ev('a', 'c1', '2026-10-20T10:00:00Z'), ev('b', 'c2', '2026-10-20T11:00:00Z'), ev('c', 'c1', '2026-10-20T12:00:00Z'), ev('d', undefined, '2026-10-20T13:00:00Z')];
		expect(eventsForCluster(events, 'c1').map((e) => e.id)).toEqual(['c', 'a']);
		expect(eventsForCluster(events, 'c1', 1).map((e) => e.id)).toEqual(['c']);
		expect(eventsForCluster(events, 'none')).toEqual([]);
	});
});

describe('backup policy form', () => {
	it('starts from defaults and from a stored policy', () => {
		expect(policyFormOf(undefined)).toMatchObject({ enabled: false, schedule: '0 2 * * *', timezone: 'UTC', missed: 'skip', method: 'physical', retentionEnabled: false });
		expect(policyFormOf({ enabled: true, schedule: '*/30 * * * *', timezone: 'Europe/Berlin', missed: 'run-once' })).toMatchObject({ enabled: true, missed: 'run-once' });
	});
	it('validates the cron shape and the time zone', () => {
		const f = policyFormOf(undefined);
		expect(validatePolicy(f)).toEqual([]);
		expect(validatePolicy({ ...f, schedule: '0 2 * *' })).toEqual(['schedule']);
		expect(validatePolicy({ ...f, schedule: '0 2 * * mon' })).toEqual(['schedule']);
		expect(validatePolicy({ ...f, schedule: '0,30 1-5 * * 1-5' })).toEqual([]);
		expect(validatePolicy({ ...f, schedule: '*/15 * * * *' })).toEqual([]);
		expect(validatePolicy({ ...f, timezone: 'Mars/Base' })).toEqual(['timezone']);
		expect(validatePolicy({ ...f, timezone: '' })).toEqual([]);
	});
	it('builds the request with normalised whitespace and the physical method', () => {
		const base = policyFormOf(undefined);
		expect(buildPolicy({ ...base, enabled: true, schedule: ' 0   2 * * * ', timezone: ' Europe/Berlin ' })).toEqual({
			enabled: true,
			schedule: '0 2 * * *',
			timezone: 'Europe/Berlin',
			missed: 'skip',
			method: 'physical'
		});
		expect(buildPolicy({ ...base, timezone: '' })).not.toHaveProperty('timezone');
	});
	it('requires databases for the logical method and sends them de-duplicated', () => {
		const f = { ...policyFormOf(undefined), method: 'logical' as const };
		expect(validatePolicy(f)).toEqual(['databases']);
		expect(validatePolicy({ ...f, databases: 'app; bad name!' })).toEqual(['databases']);
		const ok = { ...f, databases: 'app, shop app\nlogs' };
		expect(validatePolicy(ok)).toEqual([]);
		expect(buildPolicy(ok)).toMatchObject({ method: 'logical', databases: ['app', 'shop', 'logs'] });
		expect(buildPolicy({ ...ok, method: 'physical' })).not.toHaveProperty('databases');
	});
	it('validates retention counts and omits the block when it is off', () => {
		const f = policyFormOf(undefined);
		expect(buildPolicy(f)).not.toHaveProperty('retention');
		const r = { ...f, retentionEnabled: true };
		expect(validatePolicy(r)).toEqual([]);
		expect(buildPolicy(r).retention).toEqual({ keepLast: 7, keepDaily: 0, keepWeekly: 0 });
		expect(validatePolicy({ ...r, keepLast: '0' })).toEqual(['keepLast']);
		expect(validatePolicy({ ...r, keepDaily: '-1', keepWeekly: '1001' })).toEqual(['keepDaily', 'keepWeekly']);
		expect(validatePolicy({ ...f, keepLast: 'x' })).toEqual([]);
	});
	it('round-trips a stored logical policy with retention', () => {
		const stored = { enabled: true, schedule: '0 3 * * *', method: 'logical' as const, databases: ['app'], retention: { keepLast: 3, keepDaily: 7, keepWeekly: 4 } };
		expect(isBackupSummary({ cluster: 'c', backupPolicy: stored })).toBe(true);
		expect(isBackupSummary({ cluster: 'c', backupPolicy: { ...stored, retention: { keepDaily: 1 } } })).toBe(false);
		const f = policyFormOf(stored);
		expect(f).toMatchObject({ method: 'logical', databases: 'app', retentionEnabled: true, keepLast: '3' });
		expect(buildPolicy(f)).toMatchObject({ method: 'logical', databases: ['app'], retention: { keepLast: 3, keepDaily: 7, keepWeekly: 4 } });
	});
	it('accepts a backup summary with a stored policy and rejects a malformed one', () => {
		expect(isBackupSummary({ cluster: 'c', swarm: 's', backupPolicy: { enabled: true, schedule: '0 2 * * *', missed: 'skip' } })).toBe(true);
		expect(isBackupSummary({ cluster: 'c', backupPolicy: { enabled: true, schedule: '0 2 * * *', missed: 'later' } })).toBe(false);
	});
});

describe('notification settings form', () => {
	const stored = {
		minSeverity: 'error' as const,
		webhook: { enabled: true, url: 'https://hooks.example/x', secretSet: true, allowPrivate: false },
		email: { enabled: true, recipients: ['a@example.org'], addr: 'smtp.example.org:587', from: 'dcm@example.org', username: 'u', passwordSet: true }
	};
	it('never puts a stored secret into the form and sends secrets only when typed', () => {
		const f = notifyFormOf(stored);
		expect(f.webhookSecret).toBe('');
		expect(f.password).toBe('');
		const body = buildNotify(f);
		expect(body.webhook).not.toHaveProperty('secret');
		expect(body.email).not.toHaveProperty('password');
		const typed = buildNotify({ ...f, webhookSecret: 's3', password: 'pw' });
		expect(typed.webhook.secret).toBe('s3');
		expect(typed.email.password).toBe('pw');
		expect(typed.minSeverity).toBe('error');
	});
	it('validates only enabled channels', () => {
		const f = notifyFormOf(stored);
		expect(validateNotify(f)).toEqual([]);
		expect(validateNotify({ ...f, webhookUrl: 'ftp://x' })).toEqual(['webhookUrl']);
		expect(validateNotify({ ...f, webhookEnabled: false, webhookUrl: '' })).toEqual([]);
		expect(validateNotify({ ...f, recipients: 'nope', addr: 'smtp', from: 'x' })).toEqual(['recipients', 'addr', 'from']);
		expect(validateNotify({ ...f, emailEnabled: false, recipients: '', addr: '', from: '' })).toEqual([]);
	});
	it('splits recipients on commas, semicolons and whitespace', () => {
		expect(parseRecipients('a@x.org, b@x.org;c@x.org  d@x.org')).toEqual(['a@x.org', 'b@x.org', 'c@x.org', 'd@x.org']);
	});
	it('reads settings with and without the new fields and rejects a bad severity', () => {
		expect(isNotificationSettings({ webhook: { enabled: false, secretSet: false }, email: { enabled: false, recipients: [] } })).toBe(true);
		expect(isNotificationSettings(stored)).toBe(true);
		expect(isNotificationSettings({ ...stored, minSeverity: 'loud' })).toBe(false);
	});
});

describe('client calls', () => {
	const calls: { method?: string; url: string; body?: string }[] = [];
	const respond = (status: number, body: unknown): typeof fetch =>
		(async (url: string, init?: RequestInit) => {
			calls.push({ method: init?.method, url: String(url), body: init?.body as string | undefined });
			return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
		}) as typeof fetch;
	it('uses the documented paths for the lifecycle endpoints', async () => {
		calls.length = 0;
		await createClient(respond(202, { id: 'o', kind: 'cluster/delete', approval: { digest } })).deleteCluster('c 1');
		await createClient(respond(202, { id: 'o' })).startBackup('c1');
		await createClient(respond(200, { channel: 'webhook', delivered: false })).testNotification('webhook');
		await createClient(respond(200, { cluster: 'c1', swarm: 's' })).setBackupPolicy('c1', { enabled: false, schedule: '0 2 * * *' });
		expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
			'DELETE /api/v1/clusters/c%201',
			'POST /api/v1/clusters/c1/backups',
			'POST /api/v1/settings/notifications/test',
			'PUT /api/v1/clusters/c1/backup-policy'
		]);
		expect(calls[2].body).toBe('{"channel":"webhook"}');
	});
	it('keeps the approval_mismatch code of a refused approval', async () => {
		const err = await createClient(respond(409, { error: 'approval_mismatch' })).approveOperation('o', digest).catch((e) => e);
		expect(err.code).toBe('approval_mismatch');
	});
});

describe('message catalogue', () => {
	it('has the plan texts', () => {
		const keys = messageKeys('en');
		for (const k of ['planDelete1', 'planOutage7', 'planInspect4', 'phaseAwaitingApproval']) expect(keys).toContain(k);
	});
});
