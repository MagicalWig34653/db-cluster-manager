import { describe, expect, it } from 'vitest';
import { createClient, type PatchHostSummary, type PatchPolicy, type PatchWindow } from './api';
import { hostAttention, sortPatchHosts, validatePatchPolicy, validatePatchGroup, editablePatchPolicy, patchPolicyRequest } from './patch';
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
		const items = await client.patchHosts();
		expect(items.length).toBeGreaterThan(0);
		const view = await client.patchHost(items[0].host.id);
		expect(view.summary.host.id).toBe(items[0].host.id);
		expect((await client.patchHistory(items[0].host.id)).length).toBeGreaterThan(0);
	});
	it('proposes, approves with the digest and applies a plan through the mock contract', async () => {
		const client = createClient(async (url, init) => patchMock(init?.method ?? 'GET', String(url).replace(/^.*(?=\/api\/)/, ''), init?.body ? JSON.parse(String(init.body)) : undefined) ?? new Response('', { status: 404 }));
		const plan = await client.proposePatchPlan('mock-h1', { securityOnly: true });
		expect(plan.state).toBe('proposed');
		expect(plan.packages).toEqual(['openssl']);
		await expect(client.approvePatchPlan(plan.id, 'x'.repeat(64))).rejects.toMatchObject({ status: 409 });
		expect((await client.approvePatchPlan(plan.id, plan.digest)).state).toBe('approved');
		expect((await client.applyPatchPlan(plan.id)).state).toBe('succeeded');
		expect((await client.patchPlans()).some((p) => p.id === plan.id)).toBe(true);
	});
});

const mockClient = () => createClient(async (url, init) => patchMock(init?.method ?? 'GET', String(url).replace(/^.*(?=\/api\/)/, ''), init?.body ? JSON.parse(String(init.body)) : undefined) ?? new Response('', { status: 404 }));
const window: PatchWindow = { days: ['mon'], start: '02:00', durationMinutes: 60 };

describe('patch groups and policy', () => {
	it('validates a policy like the manager does', () => {
		expect(validatePatchPolicy({})).toEqual([]);
		expect(validatePatchPolicy({ reboot: 'with_plan' })).toEqual(['valPatchRebootWindow']);
		expect(validatePatchPolicy({ reboot: 'with_plan', windows: [window] })).toEqual([]);
		expect(validatePatchPolicy({ windows: [{ start: '2:00', durationMinutes: 60 }] })).toEqual(['valPatchWindowStart']);
		expect(validatePatchPolicy({ windows: [{ start: '24:00', durationMinutes: 60 }] })).toEqual(['valPatchWindowStart']);
		expect(validatePatchPolicy({ windows: [{ start: '02:00', durationMinutes: 0 }] })).toEqual(['valPatchWindowDuration']);
		expect(validatePatchPolicy({ windows: [{ start: '02:00', durationMinutes: 1441 }] })).toEqual(['valPatchWindowDuration']);
		expect(validatePatchPolicy({ windows: Array(17).fill(window) })).toEqual(['valPatchWindowCount']);
		expect(validatePatchPolicy({ reportEveryMinutes: 3 })).toEqual(['valPatchReportEvery']);
		expect(validatePatchPolicy({ reportEveryMinutes: 10081 })).toEqual(['valPatchReportEvery']);
		expect(validatePatchPolicy({ reportEveryMinutes: 0 })).toEqual([]);
	});
	it('validates a group id and name', () => {
		expect(validatePatchGroup('production', 'Production')).toEqual([]);
		expect(validatePatchGroup('', 'x')).toEqual(['valPatchGroupId']);
		expect(validatePatchGroup('has space', 'x')).toEqual(['valPatchGroupId']);
		expect(validatePatchGroup('ok', ' ')).toEqual(['valPatchGroupName']);
	});
	it('fills policy defaults for editing and drops empty fields from the request', () => {
		expect(editablePatchPolicy()).toEqual({ mode: 'report_only', containerRuntime: 'report_only', reboot: 'never', windows: [] });
		const edited = editablePatchPolicy({ mode: 'approval_required', windows: [window] });
		edited.reportEveryMinutes = null as unknown as number;
		edited.windows![0].zone = '';
		edited.windows![0].days = [];
		expect(patchPolicyRequest(edited)).toEqual({ mode: 'approval_required', containerRuntime: 'report_only', reboot: 'never', windows: [{ start: '02:00', durationMinutes: 60 }] });
		expect(window.days).toEqual(['mon']);
	});
	it('reads the default policy, which keeps container-runtime updates report-only', async () => {
		const p = await mockClient().patchPolicy();
		expect(p.mode ?? 'report_only').toBe('report_only');
		expect(p.containerRuntime ?? 'report_only').toBe('report_only');
	});
	it('replaces the default policy and reads it back', async () => {
		const client = mockClient();
		const next: PatchPolicy = { mode: 'approval_required', securityOnly: true, windows: [window], reboot: 'with_plan', containerRuntime: 'approval_required', reportEveryMinutes: 60 };
		expect(await client.savePatchPolicy(next)).toEqual(next);
		expect(await client.patchPolicy()).toEqual(next);
		await client.savePatchPolicy({});
	});
	it('creates, lists and deletes a group; a group with hosts answers 409', async () => {
		const client = mockClient();
		expect((await client.patchGroups()).map((g) => g.id)).toContain('production');
		const g = await client.savePatchGroup('staging', { name: 'Staging', policy: { mode: 'approval_required' } });
		expect(g).toMatchObject({ id: 'staging', name: 'Staging' });
		await client.deletePatchGroup('staging');
		expect((await client.patchGroups()).map((x) => x.id)).not.toContain('staging');
		await expect(client.deletePatchGroup('production')).rejects.toMatchObject({ status: 409 });
	});
	it('saves a host record with group and policy override', async () => {
		const client = mockClient();
		const h = await client.savePatchHost('mock-h2', { name: 'mock-worker-2', group: 'production', policy: { mode: 'approval_required' } });
		expect(h).toMatchObject({ id: 'mock-h2', group: 'production', policy: { mode: 'approval_required' } });
		await expect(client.savePatchHost('mock-h2', { group: 'missing' })).rejects.toMatchObject({ status: 400 });
	});
	it('maps a forbidden answer to a forbidden error', async () => {
		const client = createClient(async () => new Response(JSON.stringify({ error: 'forbidden' }), { status: 403, headers: { 'Content-Type': 'application/json' } }));
		await expect(client.savePatchPolicy({})).rejects.toMatchObject({ kind: 'forbidden', status: 403 });
	});
});
