import type { ProvisionPlanInput, ProvisionPlan } from './api';

const plans = new Map<string, ProvisionPlan>();
const json = (status: number, value: unknown) => new Response(JSON.stringify(value), { status });

export function provisionMock(method: string, path: string, input: ProvisionPlanInput & { digest?: string }): Response | undefined {
	if (method === 'POST' && path === '/api/v1/provision/plans') {
		if (!input.id || !Array.isArray(input.hosts) || input.hosts.length === 0 || input.hosts[0].role !== 'manager') return json(400, { error: 'bad_request' });
		if (plans.has(input.id)) return json(409, { error: 'conflict' });
		const checks = input.hosts.map((h) => ({ hostId: h.id, os: 'linux', portsAvailable: true, timeSynchronized: true, diskBytes: 20 * 1024 ** 3 }));
		const steps = input.hosts.map((h, i) => ({ id: `host-${String(i + 1).padStart(2, '0')}`, hostId: h.id, action: i === 0 ? 'init' : `join-${h.role}`, command: i === 0 ? 'docker swarm init' : 'docker swarm join --token "$JOIN_TOKEN" "$MANAGER_ADDRESS"' }));
		const scripts = Object.fromEntries(steps.map((s) => [s.hostId, `#!/bin/sh\nset -eu\n${s.command}\n`]));
		// Credentials are deliberately discarded by the mock as well.
		const plan: ProvisionPlan = { id: input.id, platform: input.platform, hosts: input.hosts, checks, steps, scripts, digest: 'a'.repeat(64) };
		plans.set(input.id, plan);
		return json(201, plan);
	}
	const match = path.match(/^\/api\/v1\/provision\/plans\/([^/]+)\/execute$/);
	if (method === 'POST' && match) {
		const plan = plans.get(decodeURIComponent(match[1]));
		if (!plan || plan.digest !== input.digest || plan.platform !== 'swarm') return json(409, { error: 'conflict' });
		return json(202, { id: 'mock-provision-op', kind: 'provision/swarm' });
	}
}
