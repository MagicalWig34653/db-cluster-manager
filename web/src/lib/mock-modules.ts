// Development-only fixture of GET/PUT /modules (VITE_MOCK=1). All data is invented.
import type { ModuleInfo } from './api';

const mod = (id: string, de: string, en: string, o: Partial<ModuleInfo> = {}): ModuleInfo => ({
	id, title: { de, en }, description: { de: `${de}: Beschreibung`, en: `${en}: description` },
	requires: [], enabled: true, missingConfig: [], missingModules: [], dependents: [], ...o
});

const list: ModuleInfo[] = [
	mod('patch', 'OS-Patchverwaltung', 'OS patch management', { enabled: false }),
	mod('host-backups', 'Host-Backups (restic)', 'Host backups (restic)'),
	mod('backup-copies', 'Backup-Kopien', 'Backup copies'),
	mod('image-build', 'Image-Build', 'Image build', { dependents: ['image-composer'] }),
	mod('image-composer', 'Image-Composer', 'Image composer', { requires: ['image-build'] }),
	mod('image-update', 'Image-Updates', 'Image updates', { enabled: false }),
	mod('vuln', 'Schwachstellen und KI-Antwort', 'Vulnerabilities and AI response'),
	mod('bootstrap', 'Server-Bootstrap', 'Server bootstrap', { enabled: false, missingConfig: ['manager.agentTransport'] }),
	mod('provisioning', 'SSH-Provisionierung', 'SSH provisioning'),
	mod('kubernetes', 'Kubernetes', 'Kubernetes')
];

export function modulesMock(method: string, path: string, input: { enabled?: boolean }): Response | undefined {
	const json = (status: number, value: unknown) => new Response(JSON.stringify(value), { status });
	if (path === '/api/v1/modules' && method === 'GET') return json(200, { items: list });
	const id = path.match(/^\/api\/v1\/modules\/([^/]+)$/)?.[1];
	if (!id || method !== 'PUT') return;
	const m = list.find((x) => x.id === id);
	if (!m) return json(404, { error: 'not_found' });
	if (input.enabled && m.missingConfig.length) return json(409, { error: 'module_blocked' });
	if (!input.enabled && m.dependents.length && list.some((x) => x.enabled && x.requires.includes(id))) return json(409, { error: 'module_blocked' });
	m.enabled = !!input.enabled;
	m.by = 'mock-admin';
	m.at = new Date().toISOString();
	return json(200, m);
}
