import { describe, expect, it, vi } from 'vitest';
import { createClient, type ProvisionPlanInput } from './api';
import { validProvision } from './provision';
import { provisionMock } from './mock-provision';

const input = (): ProvisionPlanInput => ({ id: 'lab', platform: 'swarm', hosts: [{ id: 'a', address: 'a.example.test', user: 'operator', role: 'manager' }], credentials: { a: { privateKey: 'synthetic-private-key', knownHosts: 'a.example.test ssh-ed25519 synthetic' } } });

describe('provision wizard model and mock', () => {
	it('requires pinned credentials and an odd manager count', () => {
		const v = input(); expect(validProvision(v)).toBe(true);
		v.hosts.push({ id: 'b', address: 'b.example.test', user: 'operator', role: 'manager' });
		v.credentials.b = { privateKey: 'synthetic', knownHosts: 'b.example.test ssh-ed25519 synthetic' };
		expect(validProvision(v)).toBe(false);
		v.hosts[1].role = 'worker'; expect(validProvision(v)).toBe(true);
		v.credentials.b.knownHosts = ''; expect(validProvision(v)).toBe(false);
	});
	it('previews without returning credentials and requires a matching digest', async () => {
		const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
			const response = provisionMock(init?.method ?? 'GET', `/api/v1${String(url).replace('/api/v1', '')}`, JSON.parse(String(init?.body ?? '{}')));
			return response ?? new Response('{}', { status: 404 });
		});
		const api = createClient(fetcher as typeof fetch);
		const plan = await api.planProvision(input());
		expect(JSON.stringify(plan)).not.toContain('synthetic-private-key');
		expect(plan.scripts.a).toContain('docker swarm init');
		await expect(api.startProvision('lab', 'b'.repeat(64))).rejects.toMatchObject({ status: 409 });
		expect((await api.startProvision('lab', plan.digest)).id).toBe('mock-provision-op');
	});
});
