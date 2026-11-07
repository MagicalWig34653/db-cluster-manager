import { describe, expect, it } from 'vitest';
import { createClient } from './api';
import { imageUpdateMock } from './mock-image-update';
import { policySummary, planActions } from './image-update';

const client = createClient(async (url, init) => imageUpdateMock(init?.method ?? 'GET', String(url).replace(/^.*(?=\/api\/)/, ''), init?.body ? JSON.parse(String(init.body)) : undefined) ?? new Response('', { status: 404 }));

describe('image updates', () => {
	it('lists services with policy and marks foreign services as opt-in', async () => {
		const services = await client.imageUpdateServices();
		expect(services.map((s) => s.service.id)).toContain('svc-mock-1');
		expect(services.find((s) => s.service.foreign)?.scope).toBe('opt-in');
	});
	it('summarises a policy and treats a missing one as manual', () => {
		expect(policySummary(undefined)).toBe('manual');
		expect(policySummary({ mode: 'auto', track: 'patch', windows: [{ days: ['sun'], start: '03:00', durationMinutes: 60 }] })).toBe('auto · patch · sun 03:00 +60 min');
		expect(policySummary({ mode: 'notify', track: 'digest' })).toBe('notify · digest');
	});
	it('offers only the actions the plan state and route allow', () => {
		expect(planActions({ state: 'proposed', route: 'service-update' })).toEqual(['approve', 'decline']);
		expect(planActions({ state: 'proposed', route: 'planner' })).toEqual(['decline']);
		expect(planActions({ state: 'proposed', route: 'report-only' })).toEqual(['decline']);
		expect(planActions({ state: 'approved', route: 'service-update' })).toEqual(['apply', 'decline']);
		expect(planActions({ state: 'succeeded', route: 'service-update', kind: 'patch' })).toEqual(['rollback']);
		expect(planActions({ state: 'succeeded', route: 'service-update', kind: 'rollback' })).toEqual([]);
		expect(planActions({ state: 'rolled_back', route: 'service-update' })).toEqual([]);
	});
	it('approves with the digest, refuses a mismatch, applies and rolls back', async () => {
		const plans = await client.imageUpdatePlans();
		const open = plans.find((p) => p.state === 'proposed' && p.route === 'service-update')!;
		await expect(client.approveImageUpdatePlan(open.id, 'x'.repeat(64))).rejects.toMatchObject({ status: 409 });
		expect((await client.approveImageUpdatePlan(open.id, open.digest)).state).toBe('approved');
		expect((await client.applyImageUpdatePlan(open.id)).state).toBe('succeeded');
		const back = await client.rollbackImageUpdatePlan(open.id);
		expect(back).toMatchObject({ kind: 'rollback', state: 'proposed' });
		expect((await client.declineImageUpdatePlan(back.id)).state).toBe('declined');
		expect((await client.imageUpdatePlan(open.id)).id).toBe(open.id);
	});
	it('refuses approval of a foreign service plan with 409', async () => {
		const foreign = (await client.imageUpdatePlans()).find((p) => p.route === 'report-only')!;
		await expect(client.approveImageUpdatePlan(foreign.id, foreign.digest)).rejects.toMatchObject({ status: 409 });
	});
	it('lists built offers from image builds as never auto', async () => {
		const built = (await client.imageUpdatePlans()).find((p) => p.kind === 'built')!;
		expect(built.source).toBe('imagebuild');
		expect(built.auto ?? false).toBe(false);
	});
	it('checks a service and reports the message', async () => {
		expect((await client.checkImageUpdate('svc-mock-1')).message).toBeTruthy();
	});
});
