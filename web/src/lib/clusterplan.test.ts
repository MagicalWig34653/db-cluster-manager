import { describe, expect, it } from 'vitest';
import { approvableDigest, clusterOperationInput, planInputProblem, scaleInBelowThree, validImage } from './clusterplan';
import { createClient, isClusterOperationPlan, isKeyRotationPreview, isMembershipResult, isProxyWriterChoice } from './api';

const d = 'a'.repeat(64);
const image = `mariadb:11.4.3@sha256:${d}`;

describe('cluster plan input', () => {
	it('validates slots and images per kind', () => {
		expect(planInputProblem('scale-out', 'Bad Slot', '', ['n1'])).toBe('slot');
		expect(planInputProblem('scale-out', 'n1', '', ['n1'])).toBe('slotExists');
		expect(planInputProblem('scale-out', 'n4', '', ['n1'])).toBeUndefined();
		expect(planInputProblem('scale-in', 'n9', '', ['n1'])).toBe('slotMissing');
		expect(planInputProblem('scale-in', 'n1', '', ['n1'])).toBeUndefined();
		expect(planInputProblem('minor-upgrade', '', 'mariadb:11.4', ['n1'])).toBe('image');
		expect(planInputProblem('minor-upgrade', '', image, ['n1'])).toBeUndefined();
		expect(planInputProblem('rolling-restart', '', '', ['n1'])).toBeUndefined();
		expect(validImage(image)).toBe(true);
	});
	it('sends no slot or image for rolling operations', () => {
		expect(clusterOperationInput('rolling-restart', 'n1', image)).toEqual({ kind: 'rolling-restart' });
		expect(clusterOperationInput('scale-out', ' n4 ', image)).toEqual({ kind: 'scale-out', slot: 'n4' });
		expect(clusterOperationInput('minor-upgrade', 'n1', image)).toEqual({ kind: 'minor-upgrade', image });
	});
	it('warns on scale-in below three nodes', () => {
		expect(scaleInBelowThree('scale-in', 3)).toBe(true);
		expect(scaleInBelowThree('scale-in', 4)).toBe(false);
		expect(scaleInBelowThree('scale-out', 3)).toBe(false);
	});
	it('approves only a previewed, confirmed digest', () => {
		expect(approvableDigest(undefined, true, false)).toBe(false);
		expect(approvableDigest(d, false, false)).toBe(false);
		expect(approvableDigest(d, true, true)).toBe(false);
		expect(approvableDigest(d, true, false)).toBe(true);
	});
});

describe('guards and calls', () => {
	it('guards the new response shapes', () => {
		expect(isClusterOperationPlan({ id: 'o', kind: 'cluster/scale-out', approval: { digest: d }, plan: {}, agentSteps: [] })).toBe(true);
		expect(isClusterOperationPlan({ id: 'o', kind: 'k', approval: {}, plan: {}, agentSteps: [] })).toBe(false);
		expect(isKeyRotationPreview({ digest: d, currentKey: 'k', started: false })).toBe(true);
		expect(isKeyRotationPreview({ digest: d })).toBe(false);
		expect(isMembershipResult({ digest: d, complete: false })).toBe(true);
		expect(isProxyWriterChoice({ epoch: 2, writer: '', pending: 'n2', members: ['p1'], candidates: ['n1', 'n2'], acks: { p1: false } })).toBe(true);
		expect(isProxyWriterChoice({ epoch: 'x', writer: '', members: [], candidates: [] })).toBe(false);
	});
	it('posts plan, rotation, membership and switch to the documented paths', async () => {
		const seen: { url: string; method?: string; body?: unknown; headers?: unknown }[] = [];
		const c = createClient((async (url: string, init?: RequestInit) => {
			seen.push({ url, method: init?.method, body: init?.body, headers: init?.headers });
			if (url.includes('operation-plan')) return new Response(JSON.stringify({ id: 'o', kind: 'k', approval: { digest: d }, plan: {}, agentSteps: [] }), { status: 202 });
			if (url.includes('data-key')) return new Response(JSON.stringify({ digest: d, currentKey: 'k', started: false }), { status: 200 });
			if (url.includes('membership')) return new Response(JSON.stringify({ digest: d, complete: false }), { status: 200 });
			return new Response(JSON.stringify({ epoch: 3, writer: '', pending: 'n2', members: [], candidates: ['n2'] }), { status: 200 });
		}) as typeof fetch);
		await c.planClusterOperation('c 1', { kind: 'rolling-restart' });
		await c.rotateDataKey();
		await c.rotateDataKey(d);
		await c.changeMembership('m 3', true);
		await c.changeMembership('m 3', true, d);
		await c.switchProxyWriter('c 1', 'n2', 'tok');
		expect(seen[0]).toMatchObject({ url: '/api/v1/clusters/c%201/operation-plan', method: 'POST', body: JSON.stringify({ kind: 'rolling-restart' }) });
		expect(seen[1].body).toBe('{}');
		expect(seen[2].body).toBe(JSON.stringify({ digest: d }));
		expect(seen[3]).toMatchObject({ url: '/api/v1/settings/replicas/m%203/membership', body: JSON.stringify({ remove: true }) });
		expect(seen[4].body).toBe(JSON.stringify({ remove: true, digest: d }));
		expect(seen[5]).toMatchObject({ url: '/api/v1/proxy-writers/c%201/switch', body: JSON.stringify({ writer: 'n2' }) });
		expect((seen[5].headers as Record<string, string>).Authorization).toBe('Bearer tok');
	});
});
