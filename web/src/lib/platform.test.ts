import { describe, expect, it } from 'vitest';
import { createClient, isDriftReport, isHost, isKeyringStatus, isMaintenancePreview, isMaintenanceStarted, type DriftReport, type KeyringStatus, type MaintenancePreview } from './api';
import {
	canEnterMaintenance,
	canLeaveMaintenance,
	driftOrdered,
	driftSummary,
	keyringSummary,
	maintenanceState,
	maintenanceView,
	replicasOrdered,
	replicaState,
	validReplicaKey
} from './platform';

const maint = { since: '2026-10-23T10:00:00Z', by: 'admin', operations: ['op1'], ready: false };

describe('host maintenance', () => {
	it('derives the state from the maintenance mark', () => {
		expect(maintenanceState({})).toBe('none');
		expect(maintenanceState({ maintenance: maint })).toBe('inProgress');
		expect(maintenanceState({ maintenance: { ...maint, ready: true } })).toBe('ready');
	});
	it('offers entering only for enrolled hosts outside maintenance, leaving only inside', () => {
		expect(canEnterMaintenance({ enrollment: 'enrolled' })).toBe(true);
		expect(canEnterMaintenance({ enrollment: 'discovered' })).toBe(false);
		expect(canEnterMaintenance({ enrollment: 'enrolled', maintenance: maint })).toBe(false);
		expect(canLeaveMaintenance({ maintenance: maint })).toBe(true);
		expect(canLeaveMaintenance({})).toBe(false);
	});
	it('accepts hosts with and without a maintenance mark and rejects a broken one', () => {
		const h = { swarm: 's', nodeId: 'n', enrollment: 'enrolled' };
		expect(isHost(h)).toBe(true);
		expect(isHost({ ...h, maintenance: maint })).toBe(true);
		expect(isHost({ ...h, maintenance: { since: 'x' } })).toBe(false);
	});
	const preview = (o: Partial<MaintenancePreview> = {}): MaintenancePreview => ({
		host: 'n1',
		end: false,
		safe: true,
		clusters: [{ cluster: 'c1', name: 'Shop DB', slots: ['n2'], steps: [{ id: 'a', kind: 'stop' }, { id: 'b', kind: 'wait' }], warnings: ['w'] }],
		...o
	});
	it('summarises a safe plan', () => {
		expect(maintenanceView(preview())).toEqual({ canStart: true, refused: [], stepCount: 2, warnings: ['w'], empty: false });
	});
	it('lists the clusters that refuse and blocks starting', () => {
		const p = preview({ safe: false, clusters: [{ cluster: 'c1', name: 'Shop DB', slots: ['n2'], steps: [], refused: 'fewer than a majority would remain' }] });
		expect(maintenanceView(p)).toMatchObject({ canStart: false, refused: [{ cluster: 'c1', name: 'Shop DB', why: 'fewer than a majority would remain' }] });
		expect(maintenanceView(preview({ clusters: [] })).empty).toBe(true);
	});
	it('guards previews and started responses', () => {
		expect(isMaintenancePreview(preview())).toBe(true);
		expect(isMaintenancePreview({ ...preview(), safe: 'yes' })).toBe(false);
		expect(isMaintenanceStarted({ host: 'n1', operations: [{ id: 'o' }], plan: preview() })).toBe(true);
		expect(isMaintenanceStarted({ host: 'n1', operations: [{}], plan: preview() })).toBe(false);
	});
	it('requests the end plan with end=true and sends the reason only when given', async () => {
		const seen: { url: string; body?: unknown }[] = [];
		const c = createClient((async (url: string, init?: RequestInit) => {
			seen.push({ url, body: init?.body });
			return new Response(JSON.stringify(init?.method === 'GET' || !init?.method ? preview() : { host: 'n1', operations: [], plan: preview() }), { status: 200 });
		}) as typeof fetch);
		await c.maintenancePlan('n 1', true);
		await c.maintenancePlan('n 1', false);
		await c.enterMaintenance('n1', 'kernel update');
		await c.enterMaintenance('n1');
		expect(seen[0].url).toBe('/api/v1/hosts/n%201/maintenance-plan?end=true');
		expect(seen[1].url).toBe('/api/v1/hosts/n%201/maintenance-plan');
		expect(seen[2].body).toBe('{"reason":"kernel update"}');
		expect(seen[3].body).toBe('{}');
	});
});

describe('drift', () => {
	const report = (o: Partial<DriftReport> = {}): DriftReport => ({
		cluster: 'c1',
		checkedAt: '2026-10-23T10:00:00Z',
		inSync: false,
		findings: [
			{ kind: 'foreign-labels', service: 'svc-a', detail: 'd1', action: 'a1' },
			{ kind: 'missing-service', slot: 'n2', detail: 'd2', action: 'a2' },
			{ kind: 'placement-changed', slot: 'n1', detail: 'd3', action: 'a3' },
			{ kind: 'not-running', slot: 'n3', detail: 'd4', action: 'a4' }
		],
		...o
	});
	it('shows services that are gone or not running first and keeps the order otherwise', () => {
		expect(driftOrdered(report().findings).map((f) => f.kind)).toEqual(['missing-service', 'not-running', 'foreign-labels', 'placement-changed']);
	});
	it('counts findings and does not believe inSync when findings exist', () => {
		expect(driftSummary(report())).toEqual({ inSync: false, count: 4, severe: 2 });
		expect(driftSummary(report({ inSync: true, findings: [] }))).toEqual({ inSync: true, count: 0, severe: 0 });
		expect(driftSummary(report({ inSync: true })).inSync).toBe(false);
	});
	it('guards reports and rejects unknown finding kinds', () => {
		expect(isDriftReport(report())).toBe(true);
		expect(isDriftReport(report({ findings: [{ kind: 'weird' as never, detail: 'x', action: 'y' }] }))).toBe(false);
		expect(isDriftReport({ ...report(), checkedAt: 'never' })).toBe(false);
	});
});

describe('keyring', () => {
	const key = 'dbcmpk1:' + 'A'.repeat(43);
	it('validates the recipient public key format', () => {
		expect(validReplicaKey(key)).toBe(true);
		expect(validReplicaKey(`  ${key}\n`)).toBe(true);
		expect(validReplicaKey('dbcmpk1:short')).toBe(false);
		expect(validReplicaKey('dbcmpk2:' + 'A'.repeat(43))).toBe(false);
		expect(validReplicaKey('dbcmpk1:' + 'A'.repeat(42) + '=')).toBe(false);
	});
	const status = (): KeyringStatus => ({
		currentKey: 'k1',
		recoverable: false,
		replicas: [
			{ id: 'm2', leader: false, publicKeyId: 'fp2', hasCurrentKey: true },
			{ id: 'm3', leader: false, hasCurrentKey: false },
			{ id: 'm1', leader: true, publicKeyId: 'fp1', hasCurrentKey: true },
			{ id: 'm4', leader: false, publicKeyId: 'fp4', hasCurrentKey: false }
		]
	});
	it('classifies replicas', () => {
		const [a, b, , d] = status().replicas;
		expect(replicaState(a)).toBe('ready');
		expect(replicaState(b)).toBe('unregistered');
		expect(replicaState(d)).toBe('noKey');
	});
	it('summarises and orders the leader first', () => {
		expect(keyringSummary(status())).toEqual({ replicas: 4, ready: 2, notReady: 2, recoverable: false });
		expect(replicasOrdered(status().replicas).map((r) => r.id)).toEqual(['m1', 'm2', 'm3', 'm4']);
	});
	it('guards the keyring and never needs key material in a response', () => {
		expect(isKeyringStatus(status())).toBe(true);
		expect(isKeyringStatus({ ...status(), recoverable: 'no' })).toBe(false);
		expect(isKeyringStatus({ currentKey: 'k', recoverable: true, replicas: [{ id: 'x' }] })).toBe(false);
	});
	it('registers a key with PUT and revokes with POST on encoded ids', async () => {
		const seen: { url: string; method?: string; body?: unknown }[] = [];
		const c = createClient((async (url: string, init?: RequestInit) => {
			seen.push({ url, method: init?.method, body: init?.body });
			return new Response(JSON.stringify(init?.method === 'PUT' ? { id: 'm 2', leader: false, hasCurrentKey: true } : { replica: 'm 2', revoked: 3 }), { status: 200 });
		}) as typeof fetch);
		await c.registerReplicaKey('m 2', key);
		expect(await c.revokeReplicaCertificates('m 2')).toEqual({ replica: 'm 2', revoked: 3 });
		expect(seen[0]).toMatchObject({ url: '/api/v1/settings/replicas/m%202/data-key-recipient', method: 'PUT' });
		expect(seen[0].body).toBe(JSON.stringify({ publicKey: key }));
		expect(seen[1]).toMatchObject({ url: '/api/v1/settings/replicas/m%202/revoke-certificates', method: 'POST' });
	});
});
