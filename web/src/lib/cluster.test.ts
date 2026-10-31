import { describe, expect, it } from 'vitest';
import type { Host } from './api';
import { buildRequest, emptyForm, parseAmount, preflight, validate } from './cluster';

const form = () => ({
	...emptyForm(),
	swarm: 's1',
	name: 'shop-db',
	image: 'mariadb:11',
	network: 'db-net',
	rootPasswordSecret: 'root-pw',
	cpuLimit: '1',
	memLimit: '1'
});

const host = (id: string, over: Partial<Host> = {}): Host => ({
	swarm: 's1',
	nodeId: id,
	enrollment: 'enrolled',
	state: 'ready',
	availability: 'active',
	nanoCpus: 4e9,
	memoryBytes: 8 * 1024 ** 3,
	...over
});

describe('cluster form', () => {
	it('parses amounts strictly', () => {
		expect(parseAmount('')).toBe(0);
		expect(parseAmount('1,5')).toBe(1.5);
		expect(parseAmount('abc')).toBeNaN();
		expect(parseAmount('-1')).toBeNaN();
	});
	it('validates required fields and reservations above limits', () => {
		expect(validate(form())).toEqual([]);
		const bad = { ...form(), name: 'Bad Name', cpuLimit: '1', cpuReservation: '2' };
		expect(validate(bad).map((e) => e.field)).toEqual(['name', 'cpuReservation']);
		expect(validate({ ...form(), mode: 'prefer' }).map((e) => e.field)).toEqual(['hosts']);
		expect(validate({ ...form(), mode: 'allowlist', hosts: ['a', 'b'] }).map((e) => e.key)).toEqual(['valAllowlistTooSmall']);
	});
	it('requires a CPU and memory limit or reservation (the planner refuses a zero demand with a bare 400)', () => {
		const none = { ...form(), cpuLimit: '', memLimit: '' };
		expect(validate(none)).toEqual([
			{ field: 'cpuLimit', key: 'valDemandRequired' },
			{ field: 'memLimit', key: 'valDemandRequired' }
		]);
		expect(validate({ ...none, cpuReservation: '0.5', memReservation: '1' })).toEqual([]);
	});
	it('builds a request in the CreateClusterRequest shape', () => {
		const f = { ...form(), size: 5 as const, mode: 'allowlist' as const, hosts: ['a', 'b', 'c', 'd', 'e'], cpuLimit: '2', memLimit: '4', memReservation: '2', reserveMem: '1', autoReplace: true };
		expect(buildRequest(f)).toEqual({
			swarm: 's1',
			name: 'shop-db',
			size: 5,
			image: 'mariadb:11',
			network: 'db-net',
			rootPasswordSecret: 'root-pw',
			resources: { limits: { milli_cpu: 2000, memory_bytes: 4 * 1024 ** 3 }, reservations: { memory_bytes: 2 * 1024 ** 3 } },
			placement: { allowlist: ['a', 'b', 'c', 'd', 'e'], reserve: { memory_bytes: 1024 ** 3 } },
			autoReplace: true
		});
	});
	it('sends binlog only when the binary log is wanted (point-in-time recovery)', () => {
		expect(buildRequest(form())).not.toHaveProperty('binlog');
		expect(buildRequest({ ...form(), binlog: true })).toMatchObject({ binlog: true });
	});
	it('preflight rejects unknown, spare, drained and over-committed hosts', () => {
		const f = { ...form(), cpuLimit: '2', memLimit: '4', reserveMem: '4.5' };
		const r = preflight(f, [
			host('ok1', { memoryBytes: 16 * 1024 ** 3 }),
			host('small'),
			host('spare', { spare: true, memoryBytes: 16 * 1024 ** 3 }),
			host('drain', { availability: 'drain' }),
			host('unk', { nanoCpus: undefined }),
			host('rev', { enrollment: 'revoked' }),
			host('other', { swarm: 's2' })
		]);
		const by = Object.fromEntries(r.verdicts.map((v) => [v.id, v]));
		expect(by.ok1.eligible).toBe(true);
		expect(by.small).toMatchObject({ reason: 'pfNoCapacity' });
		expect(by.spare).toMatchObject({ reason: 'pfSpare' });
		expect(by.drain).toMatchObject({ reason: 'pfNotActive' });
		expect(by.unk).toMatchObject({ reason: 'pfCapacityUnknown' });
		expect(by.rev).toMatchObject({ reason: 'pfNotEnrolled' });
		expect(by.other).toBeUndefined();
		expect(r.enough).toBe(false);
	});
	it('allowlist and exclusion override eligibility', () => {
		const hs = [host('a'), host('b'), host('c')];
		expect(preflight({ ...form(), mode: 'allowlist', hosts: ['a'] }, hs).eligible).toBe(1);
		expect(preflight({ ...form(), mode: 'exclude', hosts: ['a'] }, hs).eligible).toBe(2);
		expect(preflight({ ...form(), mode: 'automatic' }, hs).enough).toBe(true);
	});
});
