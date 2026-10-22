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

const GiB = 1024 ** 3;
fixtures['/api/v1/hosts'] = {
	items: [
		{ swarm: 'mock-swarm-a', nodeId: 'mock-host-1', hostname: 'mock-host-1', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 16 * GiB, agentVersion: 'mock-0.1', lastSeen: ago(1) },
		{ swarm: 'mock-swarm-a', nodeId: 'mock-host-2', hostname: 'mock-host-2', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 8 * GiB, agentVersion: 'mock-0.1', lastSeen: ago(2) },
		{ swarm: 'mock-swarm-a', nodeId: 'mock-host-3', hostname: 'mock-host-3', enrollment: 'enrolled', state: 'down', availability: 'active', nanoCpus: 2e9, memoryBytes: 4 * GiB },
		{ swarm: 'mock-swarm-a', nodeId: 'mock-host-4', hostname: 'mock-host-4', enrollment: 'discovered', state: 'ready', availability: 'active' }
	]
};
fixtures['/api/v1/backups'] = { items: [{ cluster: 'mock-galera', policy: 'mock: daily 02:00', lastBackupAt: ago(500) }] };
fixtures['/api/v1/dashboards'] = {
	items: [
		{ id: 'mock-d1', name: 'Mock personal', shared: false, panels: [{ id: 'p1', kind: 'line', title: 'Mock queries', query: 'mock_queries', unit: '/s' }, { id: 'p2', kind: 'stat', title: 'Mock connections', query: 'mock_conn' }] },
		{ id: 'mock-d2', name: 'Mock shared', shared: true, panels: [] }
	]
};
fixtures['/api/v1/notifications'] = {
	items: [
		{ id: 'mock-n1', time: ago(5), severity: 'critical', message: 'Mock: node lost', cluster: 'mock-galera', read: false, acknowledged: false },
		{ id: 'mock-n2', time: ago(90), severity: 'info', message: 'Mock: backup finished', read: true, acknowledged: true }
	]
};
fixtures['/api/v1/settings/notifications'] = {
	webhook: { enabled: true, url: 'https://example.invalid/mock', secretSet: true },
	email: { enabled: false, recipients: [] }
};
fixtures['/api/v1/clusters/mock-galera/outage-diagnosis'] = {
	candidate: 'mock-node-1',
	positions: [{ slot: 'mock-node-1', source: 'grastate', seqno: 120 }, { slot: 'mock-node-2', source: 'unknown' }],
	reasons: ['Mock: highest known seqno'],
	risks: [{ kind: 'missing-node', slot: 'mock-node-3', detail: 'Mock: no evidence from this node' }],
	requires_approval: true
};

// Mock metrics: a series with a deliberate hole so gaps are visible.
function metrics(body: { from: string; to: string; step: number }) {
	const from = Date.parse(body.from) / 1000;
	const to = Date.parse(body.to) / 1000;
	const points: [number, number | null][] = [];
	for (let t = from, i = 0; t <= to; t += body.step, i++) {
		if (i > 20 && i < 28) continue; // missing steps
		points.push([t, i > 35 && i < 40 ? null : 50 + 30 * Math.sin(i / 6)]);
	}
	return { series: [{ points }] };
}

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
		let body: any = {};
		try {
			body = JSON.parse(String(init?.body ?? '{}'));
		} catch {
			// ignore
		}
		if (url.pathname === '/api/v1/metrics/query') return json(200, metrics(body));
		if (url.pathname === '/api/v1/clusters') return json(202, { id: 'mock-op-new', kind: 'create-cluster' });
		if (/\/nodes\/[^/]+\/replace$/.test(url.pathname)) return json(409, { error: 'requires_guided_recovery' });
		// Passkey ceremonies cannot be mocked meaningfully.
		return json(503, { error: 'unavailable' });
	}
	if (method === 'PUT' && url.pathname === '/api/v1/settings/notifications') return json(200, fixtures[url.pathname]);
	if (method === 'PUT' || method === 'DELETE') return json(503, { error: 'unavailable' });
	if (url.pathname in fixtures) return json(200, fixtures[url.pathname]);
	const m = url.pathname.match(/^\/api\/v1\/clusters\/(.+)$/);
	if (m) {
		const c = clusters.find((c) => c.id === decodeURIComponent(m[1]));
		return c ? json(200, c) : json(404, { error: 'not_found' });
	}
	return json(404, { error: 'not_found' });
};
