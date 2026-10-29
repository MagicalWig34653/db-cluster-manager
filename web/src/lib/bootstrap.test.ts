import { beforeEach, describe, expect, it } from 'vitest';
import { createClient } from './api';
import { bootstrapProblems, defaultBootstrapForm, draftBody, parsePaths, progressPercent, wipeSecrets, isBootstrapView } from './bootstrap';
import { bootstrapMock, MOCK_HOST_FINGERPRINT, resetBootstrapMock } from './mock-bootstrap';

const client = () =>
	createClient((async (url: RequestInfo | URL, init?: RequestInit) => {
		const path = String(url);
		const r = bootstrapMock(init?.method ?? 'GET', path, JSON.parse(String(init?.body ?? '{}')));
		return r ?? new Response('{}', { status: 404 });
	}) as typeof fetch);

function validForm() {
	const f = defaultBootstrapForm();
	f.id = 'fresh1';
	f.address = 'fresh1.example.test';
	f.password = 'synthetic-root-password';
	return f;
}

describe('bootstrap form model', () => {
	it('accepts the default profile with credentials and finds every contradiction', () => {
		expect(bootstrapProblems(validForm())).toEqual([]);
		const f = validForm();
		f.profile.serviceUser = 'root';
		f.profile.disableRootLogin = true;
		f.profile.patch = true;
		f.profile.restic = true;
		f.profile.containerAgent = true;
		f.profile.cluster = { platform: 'swarm', role: 'join-worker', managerAddress: 'nope' };
		f.password = '';
		expect(bootstrapProblems(f)).toEqual(expect.arrayContaining([
			'bootProbCredential', 'bootProbServiceUser', 'bootProbManagerAddress', 'bootProbJoinToken', 'bootProbNativeAgent', 'bootProbResticPaths', 'bootProbDedicatedKey'
		]));
		const k = validForm();
		k.profile.cluster = { platform: 'k3s', role: 'init' };
		expect(bootstrapProblems(k)).toContain('bootProbK3sRuntime');
	});
	it('wipes every secret and keeps them out of the plan guard', () => {
		const f = validForm();
		f.privateKey = 'key';
		f.joinToken = 'tok';
		wipeSecrets(f);
		expect([f.password, f.privateKey, f.joinToken]).toEqual(['', '', '']);
		expect(draftBody(validForm()).credential).toEqual({ password: 'synthetic-root-password' });
		expect(parsePaths(' /srv/a \n\n/srv/b')).toEqual(['/srv/a', '/srv/b']);
		expect(progressPercent([{ id: 'a', title: 'a', state: 'succeeded' }, { id: 'b', title: 'b', state: 'pending' }])).toBe(50);
		expect(isBootstrapView({ id: 'x', status: 'planned', host: {}, progress: [], mainApproved: false, lockdownApproved: false, credentialsHeld: true, credential: {} })).toBe(false);
	});
});

describe('bootstrap flow against the mock', () => {
	beforeEach(resetBootstrapMock);
	it('refuses a wrong fingerprint, gates lockdown separately and never echoes secrets', async () => {
		const api = client();
		const f = validForm();
		f.profile.nativeAgent = true;
		f.profile.dedicatedKey = true;
		f.profile.disableRootLogin = true;
		const v = await api.bootstrapDraft(draftBody(f));
		expect(v.status).toBe('awaiting-fingerprint');
		expect(JSON.stringify(v)).not.toContain('synthetic-root-password');

		await expect(api.bootstrapFingerprint('fresh1', 'SHA256:wrong')).rejects.toMatchObject({ status: 409 });
		expect((await api.bootstrapGet('fresh1')).status).toBe('cancelled');

		const g = validForm();
		g.id = 'fresh2';
		g.profile.dedicatedKey = true;
		g.profile.disableRootLogin = true;
		await api.bootstrapDraft(draftBody(g));
		const planned = await api.bootstrapFingerprint('fresh2', MOCK_HOST_FINGERPRINT);
		expect(planned.status).toBe('planned');
		const plan = planned.plan!;
		await expect(api.bootstrapRun('fresh2')).rejects.toMatchObject({ status: 409 });
		await expect(api.bootstrapApprove('fresh2', plan.lockdownDigest!)).rejects.toMatchObject({ status: 409 });
		await api.bootstrapApprove('fresh2', plan.digest);
		await api.bootstrapRun('fresh2');
		let cur = await api.bootstrapGet('fresh2');
		for (let i = 0; i < 40 && cur.status === 'running'; i++) cur = await api.bootstrapGet('fresh2');
		expect(cur.status).toBe('main-complete');
		await expect(api.bootstrapRun('fresh2')).rejects.toMatchObject({ status: 409 });
		await api.bootstrapApproveLockdown('fresh2', plan.lockdownDigest!);
		await api.bootstrapRun('fresh2');
		cur = await api.bootstrapGet('fresh2');
		for (let i = 0; i < 5 && cur.status === 'running'; i++) cur = await api.bootstrapGet('fresh2');
		expect(cur.status).toBe('completed');
		expect(cur.credentialsHeld).toBe(false);
		expect(cur.progress.every((s) => s.state === 'succeeded')).toBe(true);
	});
});
