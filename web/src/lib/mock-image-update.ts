// Development-only fixtures of /image-updates/* (VITE_MOCK=1). All data is invented; the UI shows the "MOCK DATA" banner while it is active.

const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

const services: Record<string, any> = {
	'svc-mock-1': { service: { id: 'svc-mock-1', name: 'mock-web', swarm: 'mock-swarm-a', type: 'generic', image: 'registry.example.invalid/web:1.4.0' }, scope: 'owned',
		policy: { serviceId: 'svc-mock-1', mode: 'auto', track: 'patch', windows: [{ days: ['sun'], start: '03:00', durationMinutes: 60 }] },
		state: { lastCheckAt: ago(30) }, openPlan: 'iup-mock-1' },
	'svc-mock-2': { service: { id: 'svc-mock-2', name: 'mock-galera', swarm: 'mock-swarm-a', type: 'galera', image: 'mariadb:11.4.12' }, scope: 'owned', database: true,
		policy: { serviceId: 'svc-mock-2', mode: 'notify', track: 'minor' }, state: { lastCheckAt: ago(30), lastError: 'registry unreachable' }, openPlan: 'iup-mock-2' },
	'svc-mock-3': { service: { id: 'svc-mock-3', name: 'mock-foreign-proxy', swarm: 'mock-swarm-a', type: 'generic', image: 'nginx:1.27.0', foreign: true }, scope: 'opt-in',
		state: { lastCheckAt: ago(90) }, openPlan: 'iup-mock-3' }
};

const plan = (id: string, serviceId: string, o: Record<string, unknown>) => ({
	id, serviceId, serviceName: services[serviceId].service.name, swarm: 'mock-swarm-a', serviceType: services[serviceId].service.type, kind: 'patch', source: 'registry', route: 'service-update',
	from: { tag: '1.4.0', digest: 'sha256:' + 'a'.repeat(64) }, to: { tag: '1.4.1', digest: 'sha256:' + 'b'.repeat(64) }, digest: id.padEnd(64, '0'), state: 'proposed', createdBy: 'scheduler', createdAt: ago(20), ...o
});

const plans: Record<string, any> = {
	'iup-mock-1': plan('iup-mock-1', 'svc-mock-1', {}),
	'iup-mock-2': plan('iup-mock-2', 'svc-mock-2', { kind: 'minor', route: 'planner', database: true, to: { tag: '11.4.13' } }),
	'iup-mock-3': plan('iup-mock-3', 'svc-mock-3', { kind: 'digest', route: 'report-only' }),
	'iup-mock-4': plan('iup-mock-4', 'svc-mock-1', { kind: 'built', source: 'imagebuild', state: 'proposed', to: { tag: 'build-42' }, createdBy: 'admin', createdAt: ago(5) }),
	'iup-mock-5': plan('iup-mock-5', 'svc-mock-1', { state: 'rolled_back', approvedBy: 'admin', operationId: 'op-mock-9', executedBy: 'container', error: 'health gate failed, Swarm rolled back', finishedAt: ago(1400), createdAt: ago(1500) })
};
let seq = 6;

export function imageUpdateMock(method: string, path: string, body: unknown): Response | undefined {
	if (path === '/api/v1/image-updates/services' && method === 'GET') return json(200, Object.values(services));
	if (path === '/api/v1/image-updates/plans' && method === 'GET') return json(200, Object.values(plans).reverse());
	const sm = path.match(/^\/api\/v1\/image-updates\/services\/([^/]+)\/check$/);
	if (sm && method === 'POST') {
		if (!services[sm[1]]) return json(404, { error: 'not_found' });
		return json(200, { message: 'No newer image found.' });
	}
	const m = path.match(/^\/api\/v1\/image-updates\/plans\/([^/]+)(?:\/(approve|decline|apply|rollback))?$/);
	if (!m) return;
	const p = plans[m[1]];
	if (!p) return json(404, { error: 'not_found' });
	if (!m[2]) return method === 'GET' ? json(200, p) : undefined;
	if (method !== 'POST') return;
	if (m[2] === 'approve') {
		if (p.route !== 'service-update') return json(409, { error: 'policy_conflict', message: 'This plan cannot be approved here.' });
		if (p.state !== 'proposed') return json(409, { error: 'conflict' });
		if ((body as any)?.digest !== p.digest) return json(409, { error: 'approval_mismatch' });
		p.state = 'approved'; p.approvedBy = 'admin';
	} else if (m[2] === 'decline') {
		if (p.state !== 'proposed' && p.state !== 'approved') return json(409, { error: 'conflict' });
		p.state = 'declined';
	} else if (m[2] === 'apply') {
		if (p.state !== 'approved') return json(409, { error: 'conflict' });
		p.state = 'succeeded'; p.finishedAt = ago(0); p.operationId = 'op-mock-' + p.id;
	} else {
		if (p.state !== 'succeeded' || p.kind === 'rollback') return json(409, { error: 'conflict' });
		const id = `iup-mock-${seq++}`;
		plans[id] = { ...p, id, kind: 'rollback', source: 'rollback', from: p.to, to: p.from, digest: id.padEnd(64, '0'), state: 'proposed', approvedBy: undefined, finishedAt: undefined, operationId: undefined, createdAt: ago(0) };
		return json(201, plans[id]);
	}
	return json(m[2] === 'apply' ? 202 : 200, p);
}
