// Development-only fixtures of /host-sources (VITE_MOCK=1). All data is invented.

const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

const sources: Record<string, any> = {
	'src-mock-1': { id: 'src-mock-1', name: 'mock-worker-1 /etc', kind: 'managed-host', transport: 'agent', hostId: 'mock-h1', paths: ['/etc'], backend: 'sftp', repository: 'sftp:backup@example.invalid:/restic/etc',
		passwordSecret: 'secret-1', schedule: '0 3 * * *', checkSubset: '5%', lastRunAt: ago(600), lastRunStatus: 'succeeded', runtime: 'native' },
	'src-mock-2': { id: 'src-mock-2', name: 'mock-worker-2 data', kind: 'managed-host', transport: 'agent', hostId: 'mock-h2', paths: ['/srv/data'], excludes: ['*.tmp'], backend: 's3', repository: 's3:https://s3.example.invalid/bucket',
		passwordSecret: 'secret-2', lastRunAt: ago(3000), lastRunStatus: 'failed', runtimeNote: 'No agent of this host reports the restic capability.' },
	'src-mock-3': { id: 'src-mock-3', name: 'external-nas', kind: 'external-host', transport: 'ssh-plan', hostId: 'ext-1', sshAddress: 'nas.example.invalid', sshUser: 'backup', paths: ['/volume1/docs'], backend: 'rest-server',
		repository: 'rest:https://rest.example.invalid/nas', passwordSecret: 'secret-3' }
};

export function resticMock(method: string, path: string, _body: unknown): Response | undefined {
	if (path === '/api/v1/host-sources' && method === 'GET') return json(200, { items: Object.values(sources) });
	const m = path.match(/^\/api\/v1\/host-sources\/([^/]+)\/backups$/);
	if (!m || method !== 'POST') return;
	const s = sources[m[1]];
	if (!s) return json(404, { error: 'not_found' });
	if (!s.runtime && s.transport === 'agent') return json(409, { error: 'agent_runtime_unavailable', message: s.runtimeNote });
	return json(202, { id: `op-mock-${m[1]}`, kind: 'host-backup' });
}
