import { describe, expect, it } from 'vitest';
import type { Host, Operation } from './api';
import { approvalView, canAbort, canApprove, canEnroll, canResume, canRevoke, deploymentFailed, restoreHosts, validateSwarm } from './operations';

const digest = 'a'.repeat(64);
const op = (o: Partial<Operation> = {}): Operation => ({ id: 'o', kind: 'inspection-restore', state: 'pending', ...o });
const host = (h: Partial<Host> = {}): Host => ({ swarm: 's', nodeId: 'n', enrollment: 'enrolled', state: 'ready', availability: 'active', ...h });

describe('operation rules', () => {
	it('allows approval only with a digest requirement that is not yet approved', () => {
		expect(canApprove(op())).toBe(false);
		expect(canApprove(op({ approval: { digest } }))).toBe(true);
		expect(canApprove(op({ approval: { digest, approvedBy: 'admin', approvedAt: '2026-10-20T10:00:00Z' } }))).toBe(false);
		expect(canApprove(op({ approval: { digest }, state: 'failed' }))).toBe(false);
		expect(approvalView(op({ approval: { digest, approvedBy: 'admin' } }))).toMatchObject({ kind: 'approved', by: 'admin' });
		expect(approvalView(op())).toEqual({ kind: 'none' });
	});
	it('offers resume for paused (pending) operations that need no approval, abort while not finished', () => {
		expect(canResume(op())).toBe(true);
		expect(canResume(op({ approval: { digest } }))).toBe(false);
		expect(canResume(op({ state: 'running' }))).toBe(false);
		expect(canAbort(op({ state: 'running' }))).toBe(true);
		expect(canAbort(op({ state: 'succeeded' }))).toBe(false);
		expect(canAbort(op({ state: 'unknown' }))).toBe(false);
	});
	it('enrolls unmanaged hosts and revokes enrolled ones', () => {
		expect(canEnroll(host({ enrollment: 'discovered' }))).toBe(true);
		expect(canEnroll(host({ enrollment: 'revoked' }))).toBe(true);
		expect(canEnroll(host())).toBe(false);
		expect(canRevoke(host())).toBe(true);
		expect(canRevoke(host({ enrollment: 'revoked' }))).toBe(false);
	});
	it('offers only enrolled ready active hosts of the cluster Swarm for an inspection', () => {
		const hs = [host({ nodeId: 'a' }), host({ nodeId: 'b', state: 'down' }), host({ nodeId: 'c', swarm: 'x' }), host({ nodeId: 'd', enrollment: 'discovered' }), host({ nodeId: 'e', availability: 'drain' }), host({ nodeId: 'f', state: undefined })];
		expect(restoreHosts(hs, 's').map((h) => h.nodeId)).toEqual(['a']);
		expect(restoreHosts(hs, undefined)).toEqual([]);
	});
	it('validates the Swarm connect form like the API', () => {
		expect(validateSwarm({ name: 'Prod', dockerHost: 'unix:///var/run/docker.sock' })).toEqual([]);
		expect(validateSwarm({ name: 'Prod', dockerHost: 'tcp://10.0.0.1:2376' })).toEqual([]);
		expect(validateSwarm({ name: ' ', dockerHost: 'tcp://' })).toEqual(['name', 'dockerHost']);
		expect(validateSwarm({ name: 'a\nb', dockerHost: 'http://x' })).toEqual(['name', 'dockerHost']);
		expect(validateSwarm({ name: 'x', dockerHost: 'unix:///a b' })).toEqual(['dockerHost']);
	});
	it('reports failed agent deployments', () => {
		expect(deploymentFailed({ failed: [] })).toBe(false);
		expect(deploymentFailed({ failed: ['n1'] })).toBe(true);
	});
});
