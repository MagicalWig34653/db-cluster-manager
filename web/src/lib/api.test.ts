import { describe, expect, it, vi } from 'vitest';
import { ApiError, createClient, dashboardIdRE, isCluster, isEngineUpdatePlan, isEvent, isOperation, isSwarm, leaderHint, type FetchFn } from './api';

type Call = { url: string; init: RequestInit };

function server(status: number, body?: unknown, headers: Record<string, string> = {}) {
	const calls: Call[] = [];
	const fetchFn = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
		calls.push({ url: String(url), init: init ?? {} });
		const payload = body === undefined ? null : typeof body === 'string' ? body : JSON.stringify(body);
		return new Response(payload, { status, headers });
	}) as unknown as FetchFn;
	return { client: createClient(fetchFn), calls };
}

const cluster = {
	id: 'c1',
	swarm: 's1',
	name: 'Cluster',
	engine: 'mariadb',
	state: 'healthy',
	nodes: [{ id: 'n1', host: 'h1', state: 'degraded' }],
	observedAt: '2026-10-20T10:00:00Z'
};

describe('api client', () => {
	it('validates an engine update plan and encodes export approval', async () => {
		const plan = { swarm: 's', host: 'h', currentVersion: '29.8.0', targetVersion: '29.9.1', availableVersions: ['29.9.1'],
			maintenance: { host: 'h', end: false, safe: true, clusters: [] }, steps: [], warnings: [], blockers: [], rollbackNotes: [],
			pinnedManager: false, approvalRequired: true, exportable: true, digest: 'sha256:a' };
		expect(isEngineUpdatePlan(plan)).toBe(true);
		expect(isEngineUpdatePlan({ ...plan, blockers: 'none' })).toBe(false);
		const p = server(200, plan);
		expect(await p.client.engineUpdatePlan('a/b', '29.9.1')).toEqual(plan);
		expect(p.calls[0].url).toBe('/api/v1/hosts/a%2Fb/engine-update-plan?targetVersion=29.9.1');
		const b = server(200, { digest: plan.digest, script: '#!/bin/sh\n' });
		await b.client.exportEngineRunbook('h', { targetVersion: '29.9.1', digest: plan.digest, confirmPinnedManager: false });
		expect(JSON.parse(b.calls[0].init.body as string)).toEqual({ targetVersion: '29.9.1', digest: plan.digest, confirmPinnedManager: false });
	});
	it('parses health and version', async () => {
		expect(await server(200, { status: 'ok' }).client.health()).toEqual({ status: 'ok' });
		const v = { component: 'manager', version: '1', commit: 'abc', goVersion: 'go1', platform: 'linux/arm64' };
		expect(await server(200, v).client.version()).toEqual(v);
	});

	it('rejects non-2xx responses instead of inventing data', async () => {
		await expect(server(503, { error: 'unavailable' }).client.swarms()).rejects.toMatchObject({
			kind: 'unavailable',
			status: 503
		});
	});

	it('classifies 401/403/404/409/429 and never keeps server text', async () => {
		const cases: [number, unknown, string][] = [
			[401, { error: 'unauthenticated' }, 'unauthenticated'],
			[403, { error: 'forbidden' }, 'forbidden'],
			[403, { error: 'origin_rejected' }, 'origin_rejected'],
			[403, { error: 'passkey_enrollment_required' }, 'passkey_enrollment_required'],
			[403, 'not json at all', 'forbidden'],
			[404, { error: 'not_found' }, 'not_found'],
			[409, { error: 'setup_unavailable' }, 'conflict'],
			[400, { error: 'bad_request' }, 'bad_request'],
			[500, { error: 'internal_error: secret path /etc/x' }, 'server_error']
		];
		for (const [status, body, kind] of cases) {
			const err = await server(status, body).client.clusters().catch((e) => e);
			expect(err).toBeInstanceOf(ApiError);
			expect(err.kind).toBe(kind);
			expect(err.message).not.toContain('secret');
		}
	});

	it('reads Retry-After on 429', async () => {
		const err = await server(429, { error: 'rate_limited' }, { 'Retry-After': '30' })
			.client.redeemRecoveryCode('a', 'b')
			.catch((e) => e);
		expect(err).toMatchObject({ kind: 'rate_limited', retryAfter: 30 });
	});

	it('maps network failures', async () => {
		const client = createClient((async () => {
			throw new TypeError('Failed to fetch');
		}) as unknown as FetchFn);
		await expect(client.health()).rejects.toMatchObject({ kind: 'network', status: 0 });
	});

	it('rejects bodies that violate the contract', async () => {
		await expect(server(200, { items: [{ id: 's', name: 'x', state: 'great' }] }).client.swarms()).rejects.toMatchObject({
			kind: 'invalid_response'
		});
		await expect(server(200, { nope: [] }).client.events()).rejects.toMatchObject({ kind: 'invalid_response' });
		await expect(server(200, 'garbage').client.health()).rejects.toMatchObject({ kind: 'invalid_response' });
	});

	it('returns list items and single clusters', async () => {
		expect(await server(200, { items: [cluster] }).client.clusters()).toEqual([cluster]);
		const s = server(200, cluster);
		expect(await s.client.cluster('a/b')).toEqual(cluster);
		expect(s.calls[0].url).toBe('/api/v1/clusters/a%2Fb');
	});

	it('sends JSON POSTs same-origin with credentials and no custom CSRF header', async () => {
		const s = server(200, { pendingPasskeyEnrollment: true });
		await s.client.redeemRecoveryToken('admin', 'tok');
		const { url, init } = s.calls[0];
		expect(url).toBe('/api/v1/auth/recovery/token');
		expect(init.method).toBe('POST');
		expect(init.credentials).toBe('same-origin');
		expect(init.headers).toMatchObject({ 'Content-Type': 'application/json' });
		expect(JSON.parse(init.body as string)).toEqual({ accountId: 'admin', token: 'tok' });
	});

	it('accepts 204 for logout', async () => {
		await expect(server(204).client.logout()).resolves.toBeUndefined();
	});

	it('validates challenges and setup results', async () => {
		const ch = { ceremonyId: 'c', options: { challenge: 'AA' }, expiresAt: '2026-10-20T10:00:00Z' };
		expect(await server(200, ch).client.beginLogin()).toEqual(ch);
		await expect(server(200, { ceremonyId: 'c' }).client.beginLogin()).rejects.toMatchObject({
			kind: 'invalid_response'
		});
		const r = await server(201, { accountId: 'admin', recoveryCodes: null }).client.finishSetup({
			setupToken: 't',
			ceremonyId: 'c',
			credential: {} as never
		});
		expect(r.recoveryCodes).toBeNull();
	});
});

describe('guards', () => {
	it('accept optional fields when absent', () => {
		expect(isSwarm({ id: 's', name: 'n', state: 'unknown' })).toBe(true);
		expect(isCluster({ id: 'c', swarm: 's', name: 'n', state: 'unknown', nodes: [] })).toBe(true);
		expect(isOperation({ id: 'o', kind: 'k', state: 'pending' })).toBe(true);
	});
	it('reject wrong types and invalid dates', () => {
		expect(isSwarm({ id: 's', name: 'n', state: 'healthy', observedAt: 'yesterday' })).toBe(false);
		expect(isCluster({ ...cluster, nodes: [{ id: 'n', state: 'fine' }] })).toBe(false);
		expect(isCluster({ ...cluster, nodes: undefined })).toBe(false);
		expect(isEvent({ id: 'e', time: '2026-10-20T10:00:00Z', severity: 'loud', message: 'm' })).toBe(false);
		expect(isOperation(null)).toBe(false);
	});

	it('keeps only allowlisted coarse codes on errors', async () => {
		const known = await server(409, { error: 'requires_guided_recovery' }).client.replaceNode('c', 'n1').catch((e) => e);
		expect(known).toMatchObject({ kind: 'conflict', code: 'requires_guided_recovery' });
		const other = await server(409, { error: 'secret detail' }).client.replaceNode('c', 'n1').catch((e) => e);
		expect(other.code).toBeUndefined();
	});

	it('posts the create request as JSON and validates the operation reference', async () => {
		const { client, calls } = server(202, { id: 'op1' });
		const req = { swarm: 's', name: 'n', size: 3, image: 'i', network: 'x', rootPasswordSecret: 'r', resources: { limits: {}, reservations: {} }, placement: { reserve: {} } };
		expect(await client.createCluster(req)).toEqual({ id: 'op1' });
		expect(calls[0].url).toBe('/api/v1/clusters');
		expect(calls[0].init.method).toBe('POST');
		expect(JSON.parse(String(calls[0].init.body))).toEqual(req);
		await expect(server(200, {}).client.createCluster(req)).rejects.toMatchObject({ kind: 'invalid_response' });
	});

	it('accepts metric points with explicit states and rejects inconsistent ones', async () => {
		const q = { selector: 'm', start: 0, end: 1000, stepSeconds: 60 };
		const result = (points: unknown) => ({ start: 0, end: 1000, stepSeconds: 60, windowSeconds: 60, series: [{ labels: { name: 'm' }, points }], gaps: [] });
		const good = result([{ t: 1, v: 2, state: 'ok' }, { t: 2, v: null, state: 'no_data' }, { t: 3, v: null, state: 'unavailable' }]);
		expect(await server(200, good).client.queryMetrics(q)).toBeTruthy();
		for (const bad of [
			[{ t: 1, v: 'x', state: 'ok' }],
			[{ t: 1, v: null, state: 'ok' }],
			[{ t: 1, v: 5, state: 'unavailable' }],
			[{ t: 1, v: null, state: 'healthy' }],
			[[1, null]]
		]) {
			await expect(server(200, result(bad)).client.queryMetrics(q)).rejects.toMatchObject({ kind: 'invalid_response' });
		}
	});

	it('sends the metrics query with the manager field names', async () => {
		const s = server(200, { start: 0, end: 1, stepSeconds: 60, windowSeconds: 60, series: [], gaps: [] });
		const q = { selector: 'm{a="b"}', start: 0, end: 1, stepSeconds: 60, windowSeconds: 60, func: 'avg' };
		await s.client.queryMetrics(q);
		expect(s.calls[0].url).toBe('/api/v1/metrics/query');
		expect(JSON.parse(String(s.calls[0].init.body))).toEqual(q);
	});

	const dashboard = { id: 'd1', title: 'T', owner: 'me', shared: false, panels: [{ id: 'p', title: 'P', kind: 'line', selector: 'm', func: 'last', windowSeconds: 60, stepSeconds: 60, x: 0, y: 0, w: 12, h: 6 }] };
	it('creates dashboards with POST /dashboards/{id} and updates with PUT', async () => {
		const c = server(201, dashboard);
		await c.client.saveDashboard({ title: 'T', shared: false, panels: dashboard.panels as never });
		expect(c.calls[0].init.method).toBe('POST');
		expect(c.calls[0].url).toMatch(/^\/api\/v1\/dashboards\/d-[0-9a-f-]{36}$/);
		expect(dashboardIdRE.test(decodeURIComponent(c.calls[0].url.split('/').pop()!))).toBe(true);
		expect(JSON.parse(String(c.calls[0].init.body))).toEqual({ title: 'T', shared: false, panels: dashboard.panels });
		const u = server(200, dashboard);
		await u.client.saveDashboard({ title: 'T', shared: true, panels: [] }, 'd1');
		expect(u.calls[0].init.method).toBe('PUT');
		expect(u.calls[0].url).toBe('/api/v1/dashboards/d1');
	});
	it('rejects dashboards in the old name/query shape', async () => {
		const old = { id: 'd', name: 'N', shared: false, panels: [{ id: 'p', kind: 'line', title: 'x', query: 'q' }] };
		await expect(server(200, { items: [old] }).client.dashboards()).rejects.toMatchObject({ kind: 'invalid_response' });
	});

	it('keeps the new coarse codes and a plausible leader hint, drops anything else', async () => {
		const busy = await server(409, { error: 'busy' }).client.abortOperation('o').catch((e) => e);
		expect(busy).toMatchObject({ kind: 'conflict', code: 'busy' });
		const mismatch = await server(409, { error: 'approval_mismatch' }).client.approveOperation('o', 'a'.repeat(64)).catch((e) => e);
		expect(mismatch.code).toBe('approval_mismatch');
		const lead = await server(503, { error: 'not_leader', leader: 'mgr-2.local:8443' }).client.hosts().catch((e) => e);
		expect(lead).toMatchObject({ kind: 'unavailable', code: 'not_leader', leader: 'mgr-2.local:8443' });
		const evil = await server(503, { error: 'not_leader', leader: '<script>alert(1)</script>' }).client.hosts().catch((e) => e);
		expect(evil.leader).toBeUndefined();
		expect(leaderHint('a b')).toBeUndefined();
	});

	it('approves with the digest only and validates the answer', async () => {
		const digest = 'b'.repeat(64);
		const s = server(200, { digest, approvedBy: 'admin', approvedAt: '2026-10-20T10:00:00Z' });
		expect(await s.client.approveOperation('op/1', digest)).toMatchObject({ approvedBy: 'admin' });
		expect(s.calls[0].url).toBe('/api/v1/operations/op%2F1/approve');
		expect(JSON.parse(String(s.calls[0].init.body))).toEqual({ digest });
		await expect(server(200, { digest: 'short' }).client.approveOperation('o', digest)).rejects.toMatchObject({ kind: 'invalid_response' });
	});

	it('validates operations with approval and reason', () => {
		const d = 'c'.repeat(64);
		expect(isOperation({ id: 'o', kind: 'k', state: 'pending', reason: 'waiting', approval: { digest: d } })).toBe(true);
		expect(isOperation({ id: 'o', kind: 'k', state: 'pending', approval: { digest: 'zz' } })).toBe(false);
	});

	it('starts an inspection with an explicit host and validates the one-time password response', async () => {
		const started = {
			id: 'op', kind: 'inspection-restore', session: 's', backup: 'b1', host: 'n1', service: 'svc', port: 3306,
			expiresAt: '2026-10-20T14:00:00Z', approval: { digest: 'd'.repeat(64) }, inspectionAccount: 'inspect', inspectionPassword: 'pw'
		};
		const s = server(202, started);
		expect(await s.client.inspectRestore({ cluster: 'c', host: 'n1' })).toEqual(started);
		expect(JSON.parse(String(s.calls[0].init.body))).toEqual({ cluster: 'c', host: 'n1' });
		await expect(server(202, { ...started, inspectionPassword: undefined }).client.inspectRestore({ cluster: 'c' })).rejects.toMatchObject({ kind: 'invalid_response' });
	});

	it('calls host, Swarm and operation commands on the documented paths', async () => {
		const h = { swarm: 's', nodeId: 'n/1', enrollment: 'enrolled' };
		let s = server(200, h);
		await s.client.setEnrollment('n/1', 'revoke');
		expect(s.calls[0].url).toBe('/api/v1/hosts/n%2F1/revoke');
		s = server(201, { id: 'sw', name: 'Prod', state: 'unknown' });
		await s.client.connectSwarm({ name: 'Prod', dockerHost: 'unix:///var/run/docker.sock' });
		expect(s.calls[0].url).toBe('/api/v1/swarms');
		s = server(200, { created: true, labeled: ['a'], unlabeled: [], failed: [] });
		expect((await s.client.deployAgents('sw')).created).toBe(true);
		expect(s.calls[0].url).toBe('/api/v1/swarms/sw/agents/deploy');
		await expect(server(200, { created: true }).client.deployAgents('sw')).rejects.toMatchObject({ kind: 'invalid_response' });
		s = server(202, { id: 'o', kind: 'k' });
		await s.client.resumeOperation('o');
		expect(s.calls[0].url).toBe('/api/v1/operations/o/resume');
		await expect(server(204).client.syncSwarm('sw')).resolves.toBeUndefined();
	});

	it('treats a missing endpoint as not_found, not as empty data', async () => {
		await expect(server(404, { error: 'not_found' }).client.hosts()).rejects.toMatchObject({ kind: 'not_found' });
	});
});
