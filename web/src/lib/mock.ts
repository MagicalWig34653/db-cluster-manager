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
const digest = (c: string) => c.repeat(64);
const operations: Record<string, unknown>[] = [
	{
		id: 'mock-op-3',
		kind: 'mock-backup',
		swarm: 'mock-swarm-a',
		cluster: 'mock-galera',
		state: 'running',
		startedAt: ago(4),
		progress: { stepsDone: 2, stepsTotal: 5, step: 'mock-stream-backup', command: 'mock-backup', target: 'mock-host-1', state: 'running', attempts: 1, since: ago(2), updatedAt: ago(0) }
	},
	{ id: 'mock-op-4', kind: 'mock-inspection-restore', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'pending', startedAt: ago(3), reason: 'mock: waiting for approval', approval: { digest: digest('a') } },
	{ id: 'mock-op-5', kind: 'mock-replace-node', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'pending', startedAt: ago(30), reason: 'mock: quorum not certain' },
	{ id: 'mock-op-2', kind: 'mock-failover', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'succeeded', startedAt: ago(90), endedAt: ago(88), approval: { digest: digest('b'), approvedBy: 'mock-admin', approvedAt: ago(89) } },
	{ id: 'mock-op-1', kind: 'mock-deploy-agent', swarm: 'mock-swarm-b', state: 'failed', startedAt: ago(600), endedAt: ago(598) },
	{ id: 'mock-op-0', kind: 'mock-unknown-kind', state: 'unknown' }
];
const hosts: Record<string, unknown>[] = [
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-1', hostname: 'mock-host-1', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 16 * GiB, agentIdentity: true },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-2', hostname: 'mock-host-2', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 8 * GiB, agentIdentity: true },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-3', hostname: 'mock-host-3', enrollment: 'enrolled', state: 'down', availability: 'active', nanoCpus: 2e9, memoryBytes: 4 * GiB, agentIdentity: true },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-4', hostname: 'mock-host-4', enrollment: 'discovered', state: 'ready', availability: 'active' }
];
fixtures['/api/v1/backups'] = { items: [{ cluster: 'mock-galera', swarm: 'mock-swarm-a', policy: 'mock: 0 2 * * *', lastBackupAt: ago(500), nextRunAt: new Date(now + 3600_000).toISOString() }] };
const panel = (id: string, title: string, kind: string, selector: string, func = 'last') => ({ id, title, kind, selector, func, windowSeconds: 60, stepSeconds: 60, x: 0, y: 0, w: 12, h: 6 });
const dashboards: Record<string, unknown>[] = [
	{ id: 'mock-d1', title: 'Mock personal', owner: 'mock-admin', shared: false, panels: [panel('p1', 'Mock queries', 'line', 'mock_queries', 'rate'), { ...panel('p2', 'Mock connections', 'stat', 'mock_conn'), x: 12 }, { ...panel('p3', 'Mock unreadable', 'stat', 'mock_unreadable'), y: 6 }, { ...panel('p4', 'Mock per node', 'table', 'mock_node'), x: 12, y: 6 }] },
	{ id: 'mock-d2', title: 'Mock shared (other owner)', owner: 'mock-other', shared: true, panels: [] },
	{ id: 'mock-d3', title: 'Mock template', owner: 'mock-admin', shared: true, template: true, panels: [] }
];
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

// Mock metrics (milliseconds, explicit point states): holes in the answer,
// no_data steps, and a selector that is entirely unavailable.
function metrics(body: { selector: string; start: number; end: number; stepSeconds: number }) {
	const step = body.stepSeconds * 1000;
	const points: { t: number; v: number | null; state: string }[] = [];
	for (let t = body.start, i = 0; t <= body.end; t += step, i++) {
		if (i > 20 && i < 28) continue; // steps missing from the answer
		if (body.selector.startsWith('mock_unreadable')) points.push({ t, v: null, state: 'unavailable' });
		else if (i > 35 && i < 40) points.push({ t, v: null, state: 'no_data' });
		else if (i > 45 && i < 50) points.push({ t, v: null, state: 'unavailable' });
		else points.push({ t, v: 50 + 30 * Math.sin(i / 6), state: 'ok' });
	}
	const series = body.selector.startsWith('mock_node')
		? ['mock-node-1', 'mock-node-2'].map((slot) => ({ labels: { name: 'mock_node', slot }, points }))
		: [{ labels: { name: body.selector }, points }];
	return { start: body.start, end: body.end, stepSeconds: body.stepSeconds, windowSeconds: body.stepSeconds, series, gaps: [{ from: body.start, to: body.start + 5 * step, reason: 'mock: archive unreachable' }] };
}

// A new operation that waits for approval (outage recovery, cluster deletion).
function pendingOp(kind: string, cluster: string, digestChar: string, stepsTotal: number) {
	const op = {
		id: `mock-op-${kind}-${operations.length}`,
		kind,
		swarm: 'mock-swarm-a',
		cluster,
		state: 'pending',
		startedAt: new Date().toISOString(),
		reason: 'mock: waiting for approval',
		approval: { digest: digest(digestChar) },
		progress: { stepsDone: 0, stepsTotal, step: 'mock-approval-gate', state: 'pending' }
	};
	operations.unshift(op);
	return op;
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
		const path = url.pathname;
		if (path === '/api/v1/metrics/query') return json(200, metrics(body));
		if (path === '/api/v1/clusters') return json(202, { id: 'mock-op-new', kind: 'create-cluster' });
		if (/\/nodes\/[^/]+\/replace$/.test(path)) return json(409, { error: 'requires_guided_recovery' });
		const or = path.match(/^\/api\/v1\/clusters\/([^/]+)\/outage-recovery$/);
		if (or) {
			const op = pendingOp('mock-full-outage-recovery', decodeURIComponent(or[1]), 'd', 6);
			return json(202, { id: op.id, kind: op.kind, candidate: body.candidate, approval: op.approval, diagnosis: fixtures['/api/v1/clusters/mock-galera/outage-diagnosis'] });
		}
		let m = path.match(/^\/api\/v1\/hosts\/([^/]+)\/(enroll|revoke)$/);
		if (m) {
			const h = hosts.find((h) => h.nodeId === decodeURIComponent(m![1]));
			if (!h) return json(404, { error: 'not_found' });
			h.enrollment = m[2] === 'enroll' ? 'enrolled' : 'revoked';
			return json(200, h);
		}
		if (path === '/api/v1/swarms') return json(201, { id: 'mock-swarm-new', name: body.name, state: 'unknown' });
		if (/^\/api\/v1\/swarms\/[^/]+\/sync$/.test(path)) return new Response(null, { status: 204 });
		if (/^\/api\/v1\/swarms\/[^/]+\/agents\/deploy$/.test(path))
			return json(200, { serviceId: 'mock-agent-service', created: true, labeled: ['mock-host-1', 'mock-host-2'], unlabeled: [], failed: ['mock-host-3'] });
		if (path === '/api/v1/restores/inspect') {
			if (!body.host) return json(400, { error: 'bad_request' });
			const id = `mock-op-restore-${operations.length}`;
			operations.unshift({ id, kind: 'mock-inspection-restore', swarm: 'mock-swarm-a', cluster: body.cluster, state: 'pending', startedAt: ago(0), reason: 'mock: waiting for approval', approval: { digest: digest('c') } });
			return json(202, { id, kind: 'mock-inspection-restore', session: 'mock-session', backup: 'mock-backup-1', host: body.host, service: 'mock-inspect-svc', port: 3306, expiresAt: new Date(now + 4 * 3600_000).toISOString(), approval: { digest: digest('c') }, inspectionAccount: 'mock_inspector', inspectionPassword: 'mock-only-not-a-secret' });
		}
		m = path.match(/^\/api\/v1\/operations\/([^/]+)\/(resume|abort|approve)$/);
		if (m) {
			const op = operations.find((o) => o.id === decodeURIComponent(m![1]));
			if (!op) return json(404, { error: 'not_found' });
			if (m[2] === 'approve') {
				const a = op.approval as { digest: string; approvedBy?: string; approvedAt?: string } | undefined;
				if (!a) return json(409, { error: 'conflict' });
				if (body.digest !== a.digest) return json(409, { error: 'approval_mismatch' });
				a.approvedBy = 'mock-admin';
				a.approvedAt = new Date().toISOString();
				op.state = 'running';
				return json(200, a);
			}
			if (m[2] === 'abort') op.state = 'failed';
			else op.state = 'running';
			return json(202, { id: op.id, kind: op.kind });
		}
		const d = path.match(/^\/api\/v1\/dashboards\/([^/]+)$/);
		if (d) {
			const created = { id: decodeURIComponent(d[1]), owner: 'mock-admin', ...body };
			dashboards.push(created);
			return json(201, created);
		}
		// Passkey ceremonies cannot be mocked meaningfully.
		return json(503, { error: 'unavailable' });
	}
	if (method === 'PUT' && url.pathname === '/api/v1/settings/notifications') return json(200, fixtures[url.pathname]);
	const dm = url.pathname.match(/^\/api\/v1\/dashboards\/([^/]+)$/);
	if (dm && (method === 'PUT' || method === 'DELETE')) {
		const idx = dashboards.findIndex((x) => x.id === decodeURIComponent(dm[1]));
		const cur = dashboards[idx] as { owner: string; template?: boolean } | undefined;
		if (!cur || cur.owner !== 'mock-admin') return json(404, { error: 'not_found' });
		if (cur.template) return json(400, { error: 'bad_request' });
		if (method === 'DELETE') {
			dashboards.splice(idx, 1);
			return new Response(null, { status: 204 });
		}
		let body: any = {};
		try {
			body = JSON.parse(String(init?.body ?? '{}'));
		} catch {
			// ignore
		}
		dashboards[idx] = { ...cur, ...body };
		return json(200, dashboards[idx]);
	}
	const del = url.pathname.match(/^\/api\/v1\/clusters\/([^/]+)$/);
	if (del && method === 'DELETE') {
		const op = pendingOp('mock-delete-cluster', decodeURIComponent(del[1]), 'e', 4);
		return json(202, { id: op.id, kind: op.kind, approval: op.approval });
	}
	if (method === 'PUT' || method === 'DELETE') return json(503, { error: 'unavailable' });
	if (url.pathname === '/api/v1/operations') return json(200, { items: operations });
	if (url.pathname === '/api/v1/hosts') return json(200, { items: hosts });
	if (url.pathname === '/api/v1/dashboards') return json(200, { items: dashboards });
	if (url.pathname in fixtures) return json(200, fixtures[url.pathname]);
	const m = url.pathname.match(/^\/api\/v1\/clusters\/(.+)$/);
	if (m) {
		const c = clusters.find((c) => c.id === decodeURIComponent(m[1]));
		return c ? json(200, c) : json(404, { error: 'not_found' });
	}
	return json(404, { error: 'not_found' });
};
