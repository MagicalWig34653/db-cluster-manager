import { describe, expect, it } from 'vitest';
import { createClient } from './api';
import { resticMock } from './mock-restic';

const client = createClient(async (url, init) => resticMock(init?.method ?? 'GET', String(url).replace(/^.*(?=\/api\/)/, ''), undefined) ?? new Response('', { status: 404 }));

describe('host backup sources', () => {
	it('lists sources through the mock contract', async () => {
		const { items } = await client.hostSources();
		expect(items.map((s) => s.id)).toContain('src-mock-1');
		expect(items.every((s) => Array.isArray(s.paths))).toBe(true);
	});
	it('starts a backup and reports a missing agent runtime as 409', async () => {
		expect((await client.startHostBackup('src-mock-1')).id).toMatch(/^op-/);
		await expect(client.startHostBackup('src-mock-2')).rejects.toMatchObject({ status: 409 });
	});
});
