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

export function patchMock(method: string, path: string, _body: unknown): Response | undefined {
	if (path === '/api/v1/patch/hosts' && method === 'GET') return json(200, { items: Object.values(views).map((v) => v.summary) });
	const m = path.match(/^\/api\/v1\/patch\/hosts\/([^/]+)(\/poll|\/history)?$/);
	if (!m) return;
	const view = views[m[1]];
	if (!view) return json(404, { error: 'not_found' });
	if (!m[2] && method === 'GET') return json(200, view);
	if (m[2] === '/poll' && method === 'POST') return json(200, view);
	if (m[2] === '/history' && method === 'GET') return json(200, history(m[1]));
}
