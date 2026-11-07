// Development-only fixtures of the read side of /patch/hosts (VITE_MOCK=1).
// All data is invented; the UI shows the "MOCK DATA" banner while it is active.

const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

const summary = (id: string, name: string, o: Record<string, unknown> = {}) => ({
	host: { id, name, group: 'production', lastPollAt: ago(20), agentRuntime: 'native' }, os: { id: 'debian', version: '12' }, manager: 'apt',
	collectedAt: ago(20), stale: false, updatesPending: 0, securityPending: 0, rebootRequired: 'no', securityKnown: true, ...o
});

const views: Record<string, any> = {
	'mock-h1': { summary: summary('mock-h1', 'mock-worker-1', { updatesPending: 3, securityPending: 1, rebootRequired: 'yes' }),
		updates: [
			{ name: 'openssl', installed: '3.0.11-1', available: '3.0.13-1', arch: 'arm64', source: 'debian-security', security: true, class: 'os', advisories: ['DSA-0000-1'] },
			{ name: 'tzdata', installed: '2024a-0', available: '2024b-0', arch: 'all', source: 'debian', class: 'os' },
			{ name: 'docker-ce', installed: '27.0.1', available: '27.3.1', arch: 'arm64', source: 'docker', class: 'container-runtime' }
		], rebootReasons: ['linux-image-arm64'], warnings: [] },
	'mock-h2': { summary: summary('mock-h2', 'mock-worker-2'), updates: [], rebootReasons: [], warnings: [] },
	'mock-h3': { summary: summary('mock-h3', 'mock-manager-1', { stale: true, collectedAt: ago(3000), securityKnown: false, updatesPending: 5, host: { id: 'mock-h3', name: 'mock-manager-1', lastPollError: 'agent unreachable' } }),
		updates: [], rebootReasons: [], warnings: ['The package manager cannot classify security updates.'] }
};

const history = (id: string) => [
	{ hostId: id, at: ago(20), kind: 'report', result: 'ok', updatesPending: 3, securityPending: 1, rebootRequired: 'yes' },
	{ hostId: id, at: ago(1500), kind: 'apply', planId: 'plan-mock-1', result: 'succeeded', updatesPending: 0, securityPending: 0, rebootRequired: 'no' }
];

const plans: Record<string, any> = {
	'plan-mock-2': { id: 'plan-mock-2', hostId: 'mock-h1', packages: ['openssl', 'tzdata'], securityOnly: false, reboot: false, digest: 'a'.repeat(64), state: 'proposed', createdBy: 'admin', createdAt: ago(10) },
	'plan-mock-1': { id: 'plan-mock-1', hostId: 'mock-h2', packages: ['curl'], digest: 'b'.repeat(64), state: 'succeeded', createdBy: 'admin', createdAt: ago(1500), finishedAt: ago(1490) }
};
let planSeq = 3;

let defaultPolicy: Record<string, unknown> = {};
const groups: Record<string, any> = { production: { id: 'production', name: 'Production', policy: { mode: 'approval_required', securityOnly: true, windows: [{ days: ['sun'], start: '03:00', durationMinutes: 120, zone: 'Europe/Berlin' }], reboot: 'with_plan' } } };

function groupsMock(method: string, path: string, body: any): Response | undefined {
	if (path === '/api/v1/patch/policy') {
		if (method === 'PUT') defaultPolicy = body ?? {};
		return method === 'GET' || method === 'PUT' ? json(200, defaultPolicy) : undefined;
	}
	if (path === '/api/v1/patch/groups' && method === 'GET') return json(200, Object.values(groups));
	const g = path.match(/^\/api\/v1\/patch\/groups\/([^/]+)$/);
	if (g && method === 'PUT') {
		if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(g[1]) || typeof body?.name !== 'string') return json(400, { error: 'bad_request' });
		return json(200, (groups[g[1]] = { id: g[1], name: body.name, ...(body.policy ? { policy: body.policy } : {}) }));
	}
	if (g && method === 'DELETE') {
		if (!groups[g[1]]) return json(404, { error: 'not_found' });
		if (Object.values(views).some((v) => v.summary.host.group === g[1])) return json(409, { error: 'conflict' });
		delete groups[g[1]];
		return new Response(null, { status: 204 });
	}
	const h = path.match(/^\/api\/v1\/patch\/hosts\/([^/]+)$/);
	if (h && method === 'PUT') {
		if (body?.group && !groups[body.group]) return json(400, { error: 'bad_request' });
		const host = { ...(views[h[1]]?.summary.host ?? { id: h[1] }), ...body };
		if (views[h[1]]) views[h[1]].summary.host = host;
		return json(200, host);
	}
}

export function patchMock(method: string, path: string, body: unknown): Response | undefined {
	const grouped = groupsMock(method, path, body);
	if (grouped) return grouped;
	if (path === '/api/v1/patch/plans' && method === 'GET') return json(200, Object.values(plans).reverse());
	const pm = path.match(/^\/api\/v1\/patch\/plans\/([^/]+)\/(approve|decline|apply)$/);
	if (pm && method === 'POST') {
		const plan = plans[pm[1]];
		if (!plan) return json(404, { error: 'not_found' });
		if (pm[2] === 'approve') {
			if (plan.state !== 'proposed') return json(409, { error: 'conflict' });
			if ((body as any)?.digest !== plan.digest) return json(409, { error: 'approval_mismatch' });
			plan.state = 'approved'; plan.approvedBy = 'admin';
		} else if (pm[2] === 'decline') {
			if (plan.state !== 'proposed' && plan.state !== 'approved') return json(409, { error: 'conflict' });
			plan.state = 'declined';
		} else {
			if (plan.state !== 'approved') return json(409, { error: 'conflict' });
			plan.state = 'succeeded'; plan.finishedAt = ago(0);
		}
		return json(pm[2] === 'apply' ? 202 : 200, plan);
	}
	const hp = path.match(/^\/api\/v1\/patch\/hosts\/([^/]+)\/plans$/);
	if (hp && method === 'POST') {
		if (!views[hp[1]]) return json(404, { error: 'not_found' });
		const input = (body ?? {}) as { securityOnly?: boolean; reboot?: boolean };
		const names = views[hp[1]].updates.filter((u: any) => u.class !== 'container-runtime' && (!input.securityOnly || u.security)).map((u: any) => u.name);
		const id = `plan-mock-${planSeq++}`;
		plans[id] = { id, hostId: hp[1], packages: names, securityOnly: !!input.securityOnly, reboot: !!input.reboot, digest: id.padEnd(64, 'c'), state: 'proposed', createdBy: 'admin', createdAt: ago(0) };
		return json(201, plans[id]);
	}
	if (path === '/api/v1/patch/hosts' && method === 'GET') return json(200, { items: Object.values(views).map((v) => v.summary) });
	const m = path.match(/^\/api\/v1\/patch\/hosts\/([^/]+)(\/poll|\/history)?$/);
	if (!m) return;
	const view = views[m[1]];
	if (!view) return json(404, { error: 'not_found' });
	if (!m[2] && method === 'GET') return json(200, view);
	if (m[2] === '/poll' && method === 'POST') return json(200, view);
	if (m[2] === '/history' && method === 'GET') return json(200, history(m[1]));
}
