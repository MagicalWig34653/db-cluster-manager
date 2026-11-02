import { describe, expect, it } from 'vitest';
import { createClient, type PatchHostSummary } from './api';
import { hostAttention, sortPatchHosts } from './patch';
import { patchMock } from './mock-patch';

const host = (id: string, o: Partial<PatchHostSummary> = {}): PatchHostSummary => ({
	host: { id, name: id }, collectedAt: '2026-10-30T00:00:00Z', stale: false, updatesPending: 0, securityPending: 0, rebootRequired: 'no', securityKnown: true, ...o
});

describe('patch overview', () => {
	it('classifies hosts by what needs attention first', () => {
		expect(hostAttention(host('a', { host: { id: 'a', name: 'a', lastPollError: 'timeout' } }))).toBe('error');
		expect(hostAttention(host('a', { stale: true, securityPending: 3 }))).toBe('stale');
		expect(hostAttention(host('a', { securityPending: 2 }))).toBe('security');
		expect(hostAttention(host('a', { rebootRequired: 'yes' }))).toBe('reboot');
		expect(hostAttention(host('a', { updatesPending: 4 }))).toBe('updates');
		expect(hostAttention(host('a'))).toBe('ok');
	});
	it('does not call a host current when security updates cannot be classified', () => {
		expect(hostAttention(host('a', { securityKnown: false }))).toBe('unknown');
	});
	it('sorts the most urgent hosts first without mutating the input', () => {
		const list = [host('ok'), host('sec', { securityPending: 1 }), host('err', { host: { id: 'err', name: 'err', lastPollError: 'x' } }), host('upd', { updatesPending: 1 })];
		expect(sortPatchHosts(list).map((h) => h.host.id)).toEqual(['err', 'sec', 'upd', 'ok']);
		expect(list.map((h) => h.host.id)).toEqual(['ok', 'sec', 'err', 'upd']);
	});
	it('loads hosts, a host view and history through the mock contract', async () => {
		const client = createClient(async (url, init) => patchMock(init?.method ?? 'GET', String(url).replace(/^.*(?=\/api\/)/, ''), {}) ?? new Response('', { status: 404 }));
		const { items } = await client.patchHosts();
		expect(items.length).toBeGreaterThan(0);
		const view = await client.patchHost(items[0].host.id);
		expect(view.summary.host.id).toBe(items[0].host.id);
		expect((await client.patchHistory(items[0].host.id)).length).toBeGreaterThan(0);
	});
});
