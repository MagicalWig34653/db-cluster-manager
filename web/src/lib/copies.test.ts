import { describe, expect, it } from 'vitest';
import { createClient, isBackupCopy, type BackupCopy } from './api';
import { copyChoices } from './copies';

const copy = (over: Partial<BackupCopy> = {}): BackupCopy => ({
	id: 'cp-1',
	cluster: 'c1',
	swarm: 's1',
	backup: 'b1',
	target: 'dr',
	status: 'succeeded',
	objects: ['o1'],
	by: 'admin',
	startedAt: '2026-10-22T01:00:00Z',
	finishedAt: '2026-10-22T01:05:00Z',
	...over
});

describe('copy choices', () => {
	it('offers only succeeded copies of the cluster, newest first', () => {
		const list = [
			copy({ id: 'old', finishedAt: '2026-10-21T01:05:00Z' }),
			copy({ id: 'failed', status: 'failed' }),
			copy({ id: 'running', status: 'running', finishedAt: undefined }),
			copy({ id: 'other', cluster: 'c2' }),
			copy({ id: 'new', finishedAt: '2026-10-23T01:05:00Z' })
		];
		expect(copyChoices(list, 'c1').map((c) => c.id)).toEqual(['new', 'old']);
		expect(copyChoices(list, 'nope')).toEqual([]);
	});
});

describe('copy API', () => {
	it('validates copies', () => {
		expect(isBackupCopy(copy())).toBe(true);
		expect(isBackupCopy(copy({ finishedAt: undefined, error: 'x' }))).toBe(true);
		expect(isBackupCopy({ ...copy(), objects: 'o1' })).toBe(false);
		expect(isBackupCopy({ ...copy(), startedAt: 'soon' })).toBe(false);
	});
	it('lists copies and sends the copy ID with the inspection request', async () => {
		const calls: { url: string; body?: string }[] = [];
		const fetchImpl = (async (url: string, init: RequestInit) => {
			calls.push({ url: String(url), body: init.body as string | undefined });
			return new Response(JSON.stringify({ items: [copy()] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
		}) as typeof fetch;
		const client = createClient(fetchImpl);
		expect(await client.backupCopies()).toHaveLength(1);
		expect(calls[0].url).toContain('/backup-copies');
		await client.inspectRestore({ cluster: 'c1', host: 'h', copy: 'cp-1' }).catch(() => undefined);
		expect(JSON.parse(calls[1].body ?? '{}')).toEqual({ cluster: 'c1', host: 'h', copy: 'cp-1' });
	});
});
