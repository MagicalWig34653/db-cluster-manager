// Development-only fixture server, loaded by api.ts only when
// import.meta.env.DEV && VITE_MOCK=1. Production builds never import it.
// Every fixture is invented sample data; the UI shows a "MOCK DATA" banner
// whenever this module is active.

const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();

const fixtures: Record<string, unknown> = {
	'/api/v1/health': { status: 'ok' },
	'/api/v1/version': {
		component: 'manager',
		version: 'mock-dev',
		commit: 'mock',
		goVersion: 'mock',
		platform: 'mock'
	},
	'/api/v1/setup/status': { setupRequired: false },
	'/api/v1/auth/session': {
		accountId: 'mock-admin',
		restricted: false,
		pendingPasskeyEnrollment: false,
		createdAt: ago(30)
	},
	'/api/v1/swarms': {
		items: [
			{ id: 'mock-swarm-a', name: 'Mock Swarm A', state: 'healthy', observedAt: ago(1) },
			{ id: 'mock-swarm-b', name: 'Mock Swarm B', state: 'unknown' }
		]
	},
	'/api/v1/operations': {
		items: [
			{ id: 'mock-op-3', kind: 'mock-backup', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'running', startedAt: ago(4) },
			{ id: 'mock-op-2', kind: 'mock-failover', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'succeeded', startedAt: ago(90), endedAt: ago(88) },
			{ id: 'mock-op-1', kind: 'mock-deploy-agent', swarm: 'mock-swarm-b', state: 'failed', startedAt: ago(600), endedAt: ago(598) },
			{ id: 'mock-op-0', kind: 'mock-unknown-kind', state: 'unknown' }
		]
	},
	'/api/v1/events': {
		items: [
			{ id: 'mock-ev-3', time: ago(2), swarm: 'mock-swarm-a', cluster: 'mock-galera', severity: 'warning', message: 'Mock: node lag above threshold' },
			{ id: 'mock-ev-2', time: ago(60), severity: 'info', message: 'Mock: manager started' },
			{ id: 'mock-ev-1', time: ago(300), swarm: 'mock-swarm-b', severity: 'critical', message: 'Mock: swarm unreachable' },
			{ id: 'mock-ev-0', time: ago(400), severity: 'unknown', message: 'Mock: unclassified event' }
		]
	}
};

const clusters = [
	{
		id: 'mock-galera',
		swarm: 'mock-swarm-a',
		name: 'Mock Galera',
		engine: 'mariadb',
		state: 'degraded',
		observedAt: ago(2),
		nodes: [
			{ id: 'mock-node-1', host: 'mock-host-1', state: 'healthy' },
			{ id: 'mock-node-2', host: 'mock-host-2', state: 'healthy' },
			{ id: 'mock-node-3', host: 'mock-host-3', state: 'unavailable' }
		]
	},
	{ id: 'mock-empty', swarm: 'mock-swarm-b', name: 'Mock without observation', state: 'unknown', nodes: [] }
];
fixtures['/api/v1/clusters'] = { items: clusters };

const json = (status: number, body: unknown) =>
	new Response(body === undefined ? null : JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});

export const mockFetch: typeof fetch = async (input, init) => {
	const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href);
	const method = (init?.method ?? 'GET').toUpperCase();
	await new Promise((r) => setTimeout(r, 150));
	if (method === 'POST') {
		if (url.pathname === '/api/v1/auth/logout') return new Response(null, { status: 204 });
		// Passkey ceremonies cannot be mocked meaningfully.
		return json(503, { error: 'unavailable' });
	}
	if (url.pathname in fixtures) return json(200, fixtures[url.pathname]);
	const m = url.pathname.match(/^\/api\/v1\/clusters\/(.+)$/);
	if (m) {
		const c = clusters.find((c) => c.id === decodeURIComponent(m[1]));
		return c ? json(200, c) : json(404, { error: 'not_found' });
	}
	return json(404, { error: 'not_found' });
};
