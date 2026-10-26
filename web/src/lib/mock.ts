import { mockServicePlan as servicePlan, mockUpdatePlan } from './mock-services';
import type { ServiceInput } from './api';
// Development-only fixture server, loaded by api.ts only when
// import.meta.env.DEV && VITE_MOCK=1. Production builds never import it.
// Every fixture is invented sample data; the UI shows a "MOCK DATA" banner
// whenever this module is active.

import { kubernetesMock } from './mock-kubernetes';
import { provisionMock } from './mock-provision';
import { vulnMock } from './mock-vuln';

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
		progress: { stepsDone: 2, stepsTotal: 5, step: 'mock-stream-backup', command: 'mock-backup', target: 'mock-host-1', state: 'running', attempts: 2, waiting: 'mock: waiting for the stream to finish', lastError: 'mock: agent unreachable (connection refused), retry scheduled', polls: 3, since: ago(2), updatedAt: ago(0) }
	},
	{ id: 'mock-op-4', kind: 'mock-inspection-restore', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'pending', startedAt: ago(3), reason: 'mock: waiting for approval', approval: { digest: digest('a') } },
	{ id: 'mock-op-5', kind: 'mock-replace-node', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'pending', startedAt: ago(30), reason: 'mock: quorum not certain' },
	{ id: 'mock-op-2', kind: 'mock-failover', swarm: 'mock-swarm-a', cluster: 'mock-galera', state: 'succeeded', startedAt: ago(90), endedAt: ago(88), approval: { digest: digest('b'), approvedBy: 'mock-admin', approvedAt: ago(89) } },
	{ id: 'mock-op-1', kind: 'mock-deploy-agent', swarm: 'mock-swarm-b', state: 'failed', startedAt: ago(600), endedAt: ago(598) },
	{ id: 'mock-op-0', kind: 'mock-unknown-kind', state: 'unknown' }
];
const hosts: Record<string, unknown>[] = [
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-1', hostname: 'mock-host-1', role: 'manager', engineVersion: '29.8.0', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 16 * GiB, agentIdentity: true, agentVersion: '0.0.0-mock', agentSeenAt: ago(0) },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-2', hostname: 'mock-host-2', role: 'worker', engineVersion: '29.8.0', enrollment: 'enrolled', state: 'ready', availability: 'active', nanoCpus: 4e9, memoryBytes: 8 * GiB, agentIdentity: true, agentVersion: '0.0.0-mock', agentSeenAt: ago(1), maintenance: { since: ago(5), by: 'mock-admin', reason: 'mock engine update', operations: [], ready: true } },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-3', hostname: 'mock-host-3', enrollment: 'enrolled', state: 'down', availability: 'active', nanoCpus: 2e9, memoryBytes: 4 * GiB, agentIdentity: true, agentVersion: '0.0.0-mock', agentSeenAt: ago(45) },
	{ swarm: 'mock-swarm-a', nodeId: 'mock-host-4', hostname: 'mock-host-4', enrollment: 'discovered', state: 'ready', availability: 'active' }
];
fixtures['/api/v1/backups'] = {
	items: [
		{
			cluster: 'mock-galera',
			swarm: 'mock-swarm-a',
			policy: '0 2 * * *',
			backupPolicy: { enabled: true, schedule: '0 2 * * *', timezone: 'Europe/Berlin', missed: 'skip', method: 'physical', retention: { keepLast: 7, keepDaily: 14, keepWeekly: 4 } },
			lastBackupAt: ago(500),
			nextRunAt: new Date(now + 3600_000).toISOString()
		}
	]
};
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


// ---------------------------------------------------------------------------
// Services, image builds, registries, maintenance, drift and keyring mocks.
// Invented data; the real manager implements these endpoints with fakes only.

const previousServiceSpecs = new Map<string, Record<string, unknown>>();
const pendingServiceUpdates = new Map<string, { service: Record<string, unknown>; spec: Record<string, unknown> }>();
const services: Record<string, unknown>[] = [
	{ id: 'mock-svc-1', swarm: 'mock-swarm-a', name: 'shop-web', type: 'container', spec: { image: 'nginx:1.27', replicas: 2, placement: {}, resources: { limits: {}, reservations: {} } }, createdBy: 'mock-admin', createdAt: ago(2000), rollbackable: true, health: { state: 'healthy', running: 2, desired: 2, observedAt: ago(0), reasons: [] } },
	{ id: 'mock-svc-2', swarm: 'mock-swarm-a', name: 'shop-db-files', type: 'container', spec: { image: 'registry.example.invalid/team/files:2.4', replicas: 1, volumes: [{ name: 'data', target: '/srv/files' }], placement: { hostId: 'mock-host-2' }, resources: { limits: {}, reservations: {} } }, createdBy: 'mock-admin', createdAt: ago(900), health: { state: 'degraded', running: 1, desired: 1, observedAt: ago(0), updateState: 'rollback_completed', reasons: ['Mock: update rolled back'] } }
];
services.push(
 { id: 'mock-galera', swarm: 'mock-swarm-a', name: 'Mock Galera', type: 'galera', spec: { image: 'mariadb:11.4', size: 3 }, health: { state: 'unavailable', running: 1, desired: 3, observedAt: ago(0), reasons: ['Mock: wsrep quorum unavailable'] } },
 { id: 'mock-svc-stale', swarm: 'mock-swarm-a', name: 'mock-stale-sftp', type: 'sftp', spec: { image: 'example.invalid/sftp:1', placement: { hostId: 'mock-host-2' } }, health: { state: 'unknown', running: 0, desired: 1, observedAt: ago(300), reasons: ['Mock: observation stale'] } }
);
previousServiceSpecs.set('mock-svc-1', { ...(services[0].spec as Record<string, unknown>), image: 'nginx:1.26' });
const registries: Record<string, unknown>[] = [
	{ id: 'ghcr-main', name: 'GitHub Container Registry (team)', kind: 'ghcr', username: 'ci-bot', credentialSet: true, createdBy: 'mock-admin' },
	{ id: 'local-lab', name: 'Lab registry', kind: 'generic', endpoint: 'https://registry.example.invalid', credentialSet: false, createdBy: 'mock-admin' }
];
const builds: Record<string, unknown>[] = [
	{ id: 'mock-build-2', name: 'Shop web', status: 'succeeded', specHash: 'a1b2c3d4e5f6', contextHash: 'f6e5d4c3b2a1', planDigest: digest('b'), reference: 'ghcr.io/team/shop-web:1.1.0', platforms: ['linux/amd64'], baseDigests: ['sha256:' + digest('c')], imageDigest: 'sha256:' + digest('d'), sbom: { status: 'not_generated' }, requestedBy: 'mock-admin', createdAt: ago(300), finishedAt: ago(296) },
	{ id: 'mock-build-1', name: 'Shop web', status: 'failed', specHash: 'a1b2c3d4e5f6', contextHash: '0a0b0c0d0e0f', planDigest: digest('e'), reference: 'ghcr.io/team/shop-web:1.0.0', platforms: ['linux/amd64'], sbom: { status: 'not_generated' }, error: 'mock: apk add failed (package not found)', requestedBy: 'mock-admin', createdAt: ago(1500), finishedAt: ago(1497) }
];
const replicas: Record<string, unknown>[] = [
	{ id: 'manager-1', address: '10.0.0.11:7001', leader: true, publicKeyId: 'SHA256:mock3f9a1c', registeredBy: 'mock-admin', registeredAt: ago(9000), hasCurrentKey: true },
	{ id: 'manager-2', address: '10.0.0.12:7001', leader: false, publicKeyId: 'SHA256:mock77d0e2', registeredBy: 'mock-admin', registeredAt: ago(8800), hasCurrentKey: true },
	{ id: 'manager-3', address: '10.0.0.13:7001', leader: false, hasCurrentKey: false }
];
const keyring = { currentKey: 'key-2026-10', recoverable: false, replicas };

function dockerfile(req: any) {
	const n = req.spec.nginx;
	const php = req.spec.php;
	const lines = [`FROM nginx:${n.version}-alpine@${n.baseDigest}`];
	if (php?.version) lines.push(`RUN apk add --no-cache php${php.version.replace('.', '')}-fpm ${(php.extensions ?? []).map((e: string) => 'php' + php.version.replace('.', '') + '-' + e).join(' ')}`);
	if (req.spec.extraPackages?.length) lines.push(`RUN apk add --no-cache ${req.spec.extraPackages.join(' ')}`);
	lines.push('COPY nginx/nginx.conf /etc/nginx/nginx.conf');
	if (php?.ini && Object.keys(php.ini).length) lines.push('COPY php/zz-dbcm.ini /etc/php/conf.d/zz-dbcm.ini');
	if (n.customSnippet) lines.push('COPY nginx/custom.conf /etc/nginx/custom.conf');
	lines.push('RUN nginx -t', 'USER nginx', 'EXPOSE 8080');
	return lines.join('\n') + '\n';
}

function imagePlan(req: any) {
	const d = dockerfile(req);
	return {
		specHash: 'a1b2c3d4e5f6', contextHash: '9f8e7d6c5b4a', digest: digest('9'), baseImage: `nginx:${req.spec.nginx.version}-alpine@${req.spec.nginx.baseDigest}`, platforms: req.spec.platforms ?? ['linux/amd64'],
		reference: `ghcr.io/${req.target.repository}:${req.target.tag}`, dockerfile: d,
		files: [{ path: 'nginx/nginx.conf', mode: 420, content: '' }, ...(req.spec.php?.version ? [{ path: 'php/zz-dbcm.ini', mode: 420, content: '' }] : [])],
		steps: [
			{ order: 1, title: 'Verify the pinned base image', detail: 'The digest is checked before anything is built.' },
			{ order: 2, title: 'Render the build context', detail: 'Dockerfile, nginx.conf' + (req.spec.php?.version ? ' and php ini' : '') },
			{ order: 3, title: 'Build for ' + (req.spec.platforms ?? ['linux/amd64']).join(', '), detail: 'nginx -t validates the configuration inside the build.' },
			{ order: 4, title: 'Push to the target registry', detail: `ghcr.io/${req.target.repository}:${req.target.tag}` }
		],
		warnings: ['Mock: Alpine package versions are not pinned.', 'Mock: no SBOM or vulnerability scan is generated.'],
		explanation: `Builds an nginx ${req.spec.nginx.version} image${req.spec.php?.version ? ' with PHP-FPM ' + req.spec.php.version : ''} from a digest-pinned base and pushes it as ghcr.io/${req.target.repository}:${req.target.tag}. Nothing happens before you approve this plan.`
	};
}

const driftReports: Record<string, unknown> = {
	'mock-galera': {
		cluster: 'mock-galera', checkedAt: ago(1), inSync: false,
		findings: [
			{ kind: 'foreign-labels', service: 'mock-galera-n2', detail: 'Service mock-galera-n2 carries a label the manager did not set (com.example.owner=ops).', action: 'Check whether the label is intended; remove it with docker service update --label-rm if not.' },
			{ kind: 'not-running', slot: 'mock-node-3', service: 'mock-galera-n3', detail: 'Node service has no running task and the cluster is not in maintenance.', action: 'Look at docker service ps mock-galera-n3; replace the node from the cluster page if the host is lost.' },
			{ kind: 'placement-changed', slot: 'mock-node-1', service: 'mock-galera-n1', detail: 'Placement constraint differs from the cluster record (node.hostname == mock-host-9 instead of mock-host-1).', action: 'Restore the constraint, or record the move by replacing the node.' }
		]
	},
	'mock-empty': { cluster: 'mock-empty', checkedAt: ago(1), inSync: true, findings: [] }
};

function maintenancePlan(nodeId: string, end: boolean) {
	const host = hosts.find((h) => h.nodeId === nodeId);
	if (!host) return undefined;
	const hasNode = nodeId === 'mock-host-1' || nodeId === 'mock-host-2';
	if (!hasNode) return { host: nodeId, end, safe: true, clusters: [] };
	const steps = end
		? [{ id: 'check-marker', kind: 'check-volume-marker', slot: 'mock-node-2', note: 'Verify the volume identity marker' }, { id: 'start', kind: 'start-node', slot: 'mock-node-2', note: 'Start the node and wait until Synced (IST or SST)' }]
		: [{ id: 'gate', kind: 'wait-synced', slot: 'mock-node-2', conditions: ['every node of the cluster on another host is Synced at the current size'] }, { id: 'stop', kind: 'stop-node', slot: 'mock-node-2', note: 'Stop the node gracefully' }];
	if (nodeId === 'mock-host-1' && !end) {
		return { host: nodeId, end, safe: false, clusters: [{ cluster: 'mock-galera', name: 'Mock Galera', slots: ['mock-node-1'], steps: [], refused: 'Only 1 of 3 nodes would remain outside this host: fewer than a strict majority (mock-node-3 is unavailable).' }] };
	}
	return { host: nodeId, end, safe: true, clusters: [{ cluster: 'mock-galera', name: 'Mock Galera', slots: ['mock-node-2'], steps, warnings: end ? [] : ['The cluster runs with 2 of 3 nodes during maintenance.'] }] };
}

let opSeq = 100;
function platformMock(method: string, path: string, query: URLSearchParams, body: any): Response | undefined {
	const P = '/api/v1';
	if (path === P + '/services') {
		if (method === 'GET') return json(200, { items: services });
		if (method === 'POST') {
			const plan = servicePlan('create', body);
			if (!plan.executable) return json(409, { error: 'conflict' });
			const op = pendingOp('service/create', 'mock-galera', 'f', plan.steps.length);
			if (body.type !== 'galera') (op as any).cluster = undefined;
   else { op.state = 'running'; (op as any).approval = undefined; delete (op as any).reason; }
			const created = { id: 'mock-svc-' + ++opSeq, swarm: body.swarm, name: body.name, type: body.type, spec: body.spec, createdBy: 'mock-admin', createdAt: new Date().toISOString(), createOperation: op.id };
			services.push(created);
			return json(202, { service: created.id, operation: { id: op.id, kind: op.kind }, approval: op.approval ?? { digest: '' }, plan: { ...plan, provisionalIds: false } });
		}
	}
	if (path === P + '/services/plan' && method === 'POST') return json(200, servicePlan('create', body));
	let m = path.match(/^\/api\/v1\/services\/([^/]+)\/(update-plan|update)$/);
 if (m && method === 'POST') {
  const svc = services.find(x => x.id === decodeURIComponent(m![1]));
  if (!svc) return json(404, { error: 'not_found' });
  if (!['container', 'traefik'].includes(String(svc.type))) return json(409, { error: 'conflict' });
  const plan = mockUpdatePlan(svc as unknown as ServiceInput, body, previousServiceSpecs.get(String(svc.id)));
  if (m[2] === 'update-plan') return json(200, plan);
  if (!plan.executable || svc.updateOperation) return json(409, { error: 'conflict' });
  const op = pendingOp('service/update', 'mock-galera', 'e', plan.steps.length);
  delete (op as any).cluster;
  svc.updateOperation = op.id;
  pendingServiceUpdates.set(String(op.id), { service: svc, spec: body.rollback ? previousServiceSpecs.get(String(svc.id))! : body.spec });
  return json(202, { service: svc.id, operation: { id: op.id, kind: op.kind }, approval: op.approval, plan: { ...plan, provisionalIds: false } });
 }
 m = path.match(/^\/api\/v1\/services\/([^/]+)\/delete-plan$/);
	if (m && method === 'GET') {
		const svc: any = services.find((x: any) => x.id === decodeURIComponent(m![1]));
		return svc ? json(200, servicePlan('delete', svc as ServiceInput, query.get('removeVolumes') === 'true')) : json(404, { error: 'not_found' });
	}
	m = path.match(/^\/api\/v1\/services\/([^/]+)$/);
	if (m) {
		const svc: any = services.find((x: any) => x.id === decodeURIComponent(m![1]));
		if (!svc) return json(404, { error: 'not_found' });
		if (method === 'GET') return json(200, svc);
		if (method === 'DELETE') {
			const plan = servicePlan('delete', svc as ServiceInput, !!body?.removeVolumes);
			const op = pendingOp('service/delete', 'mock-galera', '1', plan.steps.length);
			svc.deleteOperation = op.id;
			return json(202, { service: svc.id, operation: { id: op.id, kind: op.kind }, approval: op.approval ?? { digest: '' }, plan: { ...plan, provisionalIds: false } });
		}
	}
	if (path === P + '/image-builds') {
		if (method === 'GET') return json(200, builds);
		if (method === 'POST') {
			if (!/^sha256:[0-9a-f]{64}$/.test(body?.spec?.nginx?.baseDigest ?? '')) return json(400, { error: 'bad_request' });
			const plan = imagePlan(body);
			if (!body.approvalDigest) return json(200, { plan });
			if (body.approvalDigest !== plan.digest) return json(409, { error: 'approval_mismatch' });
			const job = { id: 'mock-build-' + ++opSeq, name: body.spec.name, status: 'succeeded', specHash: plan.specHash, contextHash: plan.contextHash, planDigest: plan.digest, reference: plan.reference, platforms: plan.platforms, baseDigests: [body.spec.nginx.baseDigest], imageDigest: 'sha256:' + digest('7'), sbom: { status: 'not_generated' }, requestedBy: 'mock-admin', createdAt: new Date().toISOString(), finishedAt: new Date().toISOString() };
			builds.unshift(job);
			return json(202, { job });
		}
	}
	if (path === P + '/registries') {
		if (method === 'GET') return json(200, registries);
		if (method === 'POST') {
			if (registries.some((r: any) => r.id === body.id)) return json(409, { error: 'conflict' });
			// The password is never stored or echoed back.
			const r = { id: body.id, name: body.name, kind: body.kind, ...(body.endpoint ? { endpoint: body.endpoint } : {}), ...(body.username ? { username: body.username } : {}), credentialSet: !!body.password, createdBy: 'mock-admin' };
			registries.push(r);
			return json(201, r);
		}
	}
	m = path.match(/^\/api\/v1\/hosts\/([^/]+)\/maintenance-plan$/);
	if (m && method === 'GET') {
		const plan = maintenancePlan(decodeURIComponent(m[1]), query.get('end') === 'true');
		return plan ? json(200, plan) : json(404, { error: 'not_found' });
	}
	m = path.match(/^\/api\/v1\/hosts\/([^/]+)\/engine-update-(plan|runbook)$/);
	if (m) {
		const id = decodeURIComponent(m[1]);
		const h: any = hosts.find((x) => x.nodeId === id);
		if (!h) return json(404, { error: 'not_found' });
		const target = method === 'GET' ? query.get('targetVersion') ?? '' : body.targetVersion;
		const blockers = target && target !== '29.9.1' ? ['Target has no tested compatibility entry'] : target ? [] : ['Select an available target version'];
		if (!h.maintenance?.ready) blockers.push('Enter guided host maintenance and wait until ready before runbook export');
		const plan = { swarm: h.swarm, host: id, currentVersion: h.engineVersion ?? '', targetVersion: target || undefined,
			availableVersions: ['29.9.1'], compatibility: target === '29.9.1' ? 'Docker 29.9.1 tested in the arm64 lab.' : undefined,
			maintenance: maintenancePlan(id, false) ?? { host: id, end: false, safe: true, clusters: [] },
			steps: ['Enter host maintenance and wait for ready', 'Operator drains this Swarm node', 'Operator installs the exact Docker version', 'Verify Swarm and Galera; restore active availability; end maintenance'],
			warnings: ['Mock plan: no real package source or host was queried'], blockers, rollbackNotes: ['Package downgrade support depends on the host installation method.'],
			pinnedManager: h.role === 'manager', approvalRequired: true, exportable: !!target && blockers.length === 0, digest: 'sha256:' + digest('e') };
		if (method === 'GET') return json(200, plan);
		if (method === 'POST') {
			if (body.digest !== plan.digest) return json(409, { error: 'approval_mismatch' });
			if (!plan.exportable || (plan.pinnedManager && !body.confirmPinnedManager)) return json(409, { error: 'conflict' });
			return json(200, { digest: plan.digest, script: '#!/bin/sh\n# Mock runbook; prints instructions only\nprintf "%s\\n" "Review maintenance, drain the node manually, install Docker manually and verify."\n' });
		}
	}
	m = path.match(/^\/api\/v1\/hosts\/([^/]+)\/maintenance$/);
	if (m && (method === 'POST' || method === 'DELETE')) {
		const id = decodeURIComponent(m[1]);
		const h: any = hosts.find((x) => x.nodeId === id);
		if (!h) return json(404, { error: 'not_found' });
		const end = method === 'DELETE';
		const plan: any = maintenancePlan(id, end);
		if (end ? !h.maintenance : h.maintenance || !plan.safe) return json(409, { error: 'conflict' });
		const ops = plan.clusters.map((c: any) => { const o = pendingOp(end ? 'host/maintenance-end' : 'host/maintenance', c.cluster, '2', c.steps.length); (o as any).state = 'running'; delete (o as any).approval; (o as any).reason = undefined; return { id: o.id, kind: o.kind }; });
		if (end) delete h.maintenance;
		else h.maintenance = { since: new Date().toISOString(), by: 'mock-admin', reason: body?.reason, operations: ops.map((o: any) => o.id), ready: false };
		return json(202, { host: id, operations: ops, plan });
	}
	m = path.match(/^\/api\/v1\/clusters\/([^/]+)\/drift$/);
	if (m && method === 'GET') {
		const r = driftReports[decodeURIComponent(m[1])];
		return r ? json(200, r) : json(404, { error: 'not_found' });
	}
	if (path === P + '/settings/replicas' && method === 'GET') return json(200, keyring);
	m = path.match(/^\/api\/v1\/settings\/replicas\/([^/]+)\/data-key-recipient$/);
	if (m && method === 'PUT') {
		const r: any = replicas.find((x: any) => x.id === decodeURIComponent(m![1]));
		if (!r) return json(400, { error: 'bad_request' });
		if (!/^dbcmpk1:[A-Za-z0-9_-]{43}$/.test(body?.publicKey ?? '')) return json(400, { error: 'bad_request' });
		Object.assign(r, { publicKeyId: 'SHA256:mock' + String(body.publicKey).slice(8, 14), registeredBy: 'mock-admin', registeredAt: new Date().toISOString(), hasCurrentKey: true });
		return json(200, r);
	}
	m = path.match(/^\/api\/v1\/settings\/replicas\/([^/]+)\/revoke-certificates$/);
	if (m && method === 'POST') return json(200, { replica: decodeURIComponent(m[1]), revoked: 2 });
	if (path === P + '/settings/data-key/rotate' && method === 'POST') {
		const want = digest('r');
		if (!body?.digest) return json(200, { digest: want, currentKey: keyring.currentKey, started: false });
		if (body.digest !== want) return json(409, { error: 'conflict' });
		keyring.currentKey = 'key-2026-11';
		return json(200, { digest: want, currentKey: keyring.currentKey, started: true });
	}
	m = path.match(/^\/api\/v1\/settings\/replicas\/([^/]+)\/membership$/);
	if (m && method === 'POST') {
		const want = digest(body?.remove ? 'm' : 'n');
		if (!body?.digest) return json(200, { digest: want, complete: false });
		return body.digest === want ? json(200, { digest: want, complete: true }) : json(409, { error: 'conflict' });
	}
	m = path.match(/^\/api\/v1\/clusters\/([^/]+)\/operation-plan$/);
	if (m && method === 'POST') {
		const cluster = decodeURIComponent(m[1]);
		const kind = String(body?.kind ?? '');
		if (!['scale-out', 'scale-in', 'rolling-restart', 'minor-upgrade'].includes(kind)) return json(400, { error: 'bad_request' });
		const slots = ['mock-node-1', 'mock-node-2', 'mock-node-3'];
		const slot = String(body?.slot ?? '');
		const targets = kind === 'scale-out' ? [slot] : kind === 'scale-in' ? [slot] : slots;
		const steps = targets.flatMap((s, i) => [
			{ id: `s${i}-gate`, kind: 'gate-synced', slot: s, note: 'Require fresh Synced evidence in one Primary component', conditions: ['wsrep_local_state = Synced', 'No other operation running'] },
			{ id: `s${i}-act`, kind: kind === 'scale-out' ? 'add-node' : kind === 'scale-in' ? 'remove-node' : kind === 'minor-upgrade' ? 'upgrade-node' : 'restart-node', slot: s }
		]);
		const op = pendingOp(`cluster/${kind}`, cluster, 'p', steps.length);
		return json(202, {
			id: op.id,
			kind: op.kind,
			approval: op.approval,
			plan: { steps, warnings: kind === 'minor-upgrade' ? ['Patch upgrade keeps the MariaDB series; rollback needs a backup.'] : ['Live gates are rechecked before every stop.'] },
			agentSteps: steps.map((s) => ({ id: s.id, command: s.kind, target: s.slot }))
		});
	}
	m = path.match(/^\/api\/v1\/proxy-writers\/([^/]+)\/switch$/);
	if (m && method === 'POST') {
		return json(200, { epoch: 4, writer: '', pending: String(body?.writer ?? ''), members: ['proxy-1', 'proxy-2'], candidates: ['mock-node-1', 'mock-node-2', 'mock-node-3'], acks: { 'proxy-1': true, 'proxy-2': false } });
	}
	return undefined;
}

export const mockFetch: typeof fetch = async (input, init) => {
	const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href);
	const method = (init?.method ?? 'GET').toUpperCase();
	await new Promise((r) => setTimeout(r, 150));
	let parsed: any = {};
	try {
		if (init?.body) parsed = JSON.parse(String(init.body));
	} catch {
		// ignore
	}
	const platform = provisionMock(method, url.pathname, parsed) ?? platformMock(method, url.pathname, url.searchParams, parsed) ?? vulnMock(method, url.pathname, parsed) ?? kubernetesMock(method, url.pathname, parsed);
	if (platform) return platform;
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
    const pending = pendingServiceUpdates.get(String(op.id));
    if (pending) {
     previousServiceSpecs.set(String(pending.service.id), pending.service.spec as Record<string, unknown>);
     pending.service.spec = pending.spec; pending.service.rollbackable = true;
     delete pending.service.updateOperation; pendingServiceUpdates.delete(String(op.id));
     op.state = 'succeeded';
    } else op.state = 'running';
				return json(200, a);
			}
			if (m[2] === 'abort') { op.state = 'aborted'; const pending = pendingServiceUpdates.get(String(op.id)); if (pending) { delete pending.service.updateOperation; pendingServiceUpdates.delete(String(op.id)); } }
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
