import { describe, expect, it, vi } from 'vitest';
import { ApiError, createClient, isCluster, isEvent, isOperation, isSwarm, type FetchFn } from './api';

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
});
