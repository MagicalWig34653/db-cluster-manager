import { describe, expect, it } from 'vitest';
import { createClient, isServicePlan, isServiceStarted, type ServicePlan } from './api';
import { buildServiceInput, emptyServiceForm, imagePinned, parseEnv, parseVolumes, planView, routeName, shortDigest, validateServiceForm } from './services';

const container = () => ({ ...emptyServiceForm('container'), swarm: 's1', name: 'web-app', image: 'nginx:1.27' });
const traefik = () => ({
	...emptyServiceForm('traefik'),
	swarm: 's1',
	name: 'edge',
	hostId: 'node-1',
	acmeEmail: 'ops@example.org',
	routes: [{ host: 'app.example.org', backend: 'http://web-app:8080', tls: true }]
});

describe('image pinning', () => {
	it('accepts tags other than latest and digests only', () => {
		expect(imagePinned('nginx:1.27')).toBe(true);
		expect(imagePinned('registry.example:5000/team/app:2')).toBe(true);
		expect(imagePinned('app@sha256:' + 'a'.repeat(64))).toBe(true);
		expect(imagePinned('nginx')).toBe(false);
		expect(imagePinned('nginx:latest')).toBe(false);
		expect(imagePinned('registry.example:5000/app')).toBe(false);
		expect(imagePinned('nginx: 1')).toBe(false);
		expect(imagePinned('')).toBe(false);
	});
});

describe('environment and volume parsing', () => {
	it('flags secret-looking names with literal values but allows the _FILE convention', () => {
		expect(parseEnv('DB_PASSWORD=hunter2').secret).toBe(true);
		expect(parseEnv('API_KEY=x\nTZ=UTC').secret).toBe(true);
		expect(parseEnv('DB_PASSWORD_FILE=/run/secrets/db').secret).toBe(false);
		expect(parseEnv('# comment\nTZ=UTC\nLANG=C').vars).toEqual([{ name: 'TZ', value: 'UTC' }, { name: 'LANG', value: 'C' }]);
	});
	it('rejects malformed and duplicate variables', () => {
		expect(parseEnv('NOEQUALS').invalid).toBe(true);
		expect(parseEnv('1BAD=x').invalid).toBe(true);
		expect(parseEnv('A=1\nA=2').invalid).toBe(true);
		expect(parseEnv('A=b=c').vars[0].value).toBe('b=c');
	});
	it('parses managed volumes and refuses host paths', () => {
		expect(parseVolumes('data:/var/lib/app\nlogs:/var/log/app:ro').volumes).toEqual([
			{ name: 'data', target: '/var/lib/app' },
			{ name: 'logs', target: '/var/log/app', readOnly: true }
		]);
		expect(parseVolumes('/srv/data:/data').invalid).toBe(true);
		expect(parseVolumes('data:relative').invalid).toBe(true);
		expect(parseVolumes('data:/a/../b').invalid).toBe(true);
		expect(parseVolumes('data:/a:rw').invalid).toBe(true);
		expect(parseVolumes('d:/a\nd:/b').invalid).toBe(true);
	});
});

describe('service form validation', () => {
	it('accepts a minimal container and a traefik request', () => {
		expect(validateServiceForm(container())).toEqual([]);
		expect(validateServiceForm(traefik())).toEqual([]);
	});
	it('reports name, image, swarm and numeric problems', () => {
		const f = { ...container(), swarm: '', name: 'Bad Name', image: 'nginx:latest', replicas: '51', cpuMilli: '0', memoryMiB: 'x' };
		expect(validateServiceForm(f)).toEqual(['swarm', 'name', 'image', 'replicas', 'cpu', 'memory']);
	});
	it('explains secrets in environment variables and volumes without a pinned host', () => {
		expect(validateServiceForm({ ...container(), env: 'TOKEN=abc' })).toEqual(['envSecret']);
		expect(validateServiceForm({ ...container(), volumes: 'data:/d' })).toEqual(['volumeHost']);
		expect(validateServiceForm({ ...container(), volumes: 'data:/d', hostId: 'n1', replicas: '2' })).toEqual(['volumeHost']);
		expect(validateServiceForm({ ...container(), volumes: 'data:/d', hostId: 'n1' })).toEqual([]);
	});
	it('requires an ACME e-mail for TLS routes and valid hosts and backends', () => {
		expect(validateServiceForm({ ...traefik(), acmeEmail: '' })).toEqual(['tlsEmail']);
		expect(validateServiceForm({ ...traefik(), acmeEmail: 'nope' })).toEqual(['email']);
		const bad = { ...traefik(), routes: [{ host: 'Bad_Host', backend: 'ftp://x', tls: false }] };
		expect(validateServiceForm(bad)).toContain('routes');
		expect(validateServiceForm({ ...traefik(), routes: [{ host: '', backend: '', tls: true }], acmeEmail: '' })).toEqual([]);
	});
});

describe('service request', () => {
	it('builds a container spec without secret values', () => {
		const r = buildServiceInput({ ...container(), env: 'TZ=UTC', secrets: 'db_pw, app_key', memoryMiB: '256', hostId: 'n1', volumes: 'data:/d' });
		expect(r).toMatchObject({ swarm: 's1', name: 'web-app', type: 'container' });
		expect(r.spec).toMatchObject({
			image: 'nginx:1.27',
			replicas: 1,
			env: [{ name: 'TZ', value: 'UTC' }],
			secrets: ['db_pw', 'app_key'],
			volumes: [{ name: 'data', target: '/d' }],
			resources: { limits: { memory_bytes: 256 * 1024 * 1024 } },
			placement: { hostId: 'n1' },
			restart: 'any'
		});
		expect(buildServiceInput(container()).spec).not.toHaveProperty('env');
	});
	it('builds a traefik spec with redirect, resolver, router and backend', () => {
		const spec = buildServiceInput(traefik()).spec as Record<string, any>;
		expect(spec.entryPoints).toEqual([
			{ name: 'web', port: 80, published: 80, redirectTo: 'websecure' },
			{ name: 'websecure', port: 443, published: 443 }
		]);
		expect(spec.certResolvers[0]).toMatchObject({ email: 'ops@example.org', staging: true, challenge: { type: 'http', entryPoint: 'web' } });
		expect(spec.routers[0]).toMatchObject({ name: 'app-example-org', entryPoints: ['websecure'], tls: { certResolver: 'letsencrypt' } });
		expect(spec.services[0]).toEqual({ name: 'app-example-org', servers: ['http://web-app:8080'] });
	});
	it('serves plain HTTP only when no route uses TLS', () => {
		const spec = buildServiceInput({ ...traefik(), routes: [{ host: 'a.example.org', backend: 'http://a:80', tls: false }] }).spec as Record<string, any>;
		expect(spec.entryPoints).toHaveLength(1);
		expect(spec.entryPoints[0]).not.toHaveProperty('redirectTo');
		expect(spec).not.toHaveProperty('certResolvers');
		expect(spec.routers[0].entryPoints).toEqual(['web']);
	});
	it('derives router names from hosts', () => {
		expect(routeName('app.example.org')).toBe('app-example-org');
	});
});

const plan = (o: Partial<ServicePlan> = {}): ServicePlan => ({
	kind: 'create',
	type: 'container',
	name: 'web-app',
	steps: [{ id: 's1', kind: 'create-service' }],
	artifacts: [{ name: 'service.json', mediaType: 'application/json', content: '{}' }],
	warnings: [],
	blockers: [],
	executable: true,
	requiresApproval: true,
	text: 'plan',
	...o
});

describe('plans and responses', () => {
	it('can start only without blockers', () => {
		expect(planView(plan()).canStart).toBe(true);
		expect(planView(plan({ blockers: ['published ports'] }))).toMatchObject({ canStart: false, blocked: true });
		expect(planView(plan({ executable: false })).canStart).toBe(false);
		expect(planView(plan())).toMatchObject({ steps: 1, artifacts: 1 });
	});
	it('guards plan and started responses', () => {
		expect(isServicePlan(plan())).toBe(true);
		expect(isServicePlan({ ...plan(), kind: 'update' })).toBe(true);
		expect(isServicePlan({ ...plan(), steps: [{ kind: 'x' }] })).toBe(false);
		const started = { service: 'svc-1', operation: { id: 'op-1' }, approval: { digest: 'a'.repeat(64) }, plan: plan() };
		expect(isServiceStarted(started)).toBe(true);
		expect(isServiceStarted({ ...started, approval: {} })).toBe(false);
	});
	it('shortens digests', () => {
		expect(shortDigest('sha256:' + 'ab'.repeat(32))).toBe('abababababab');
	});
});

describe('service client', () => {
	const respond = (status: number, body: unknown) => async () => new Response(JSON.stringify(body), { status });
	it('sends the delete body only when volumes are removed and encodes ids', async () => {
		const calls: { url: string; init?: RequestInit }[] = [];
		const c = createClient((async (url: string, init?: RequestInit) => {
			calls.push({ url, init });
			return new Response(JSON.stringify({ service: 's', operation: { id: 'o' }, approval: { digest: 'd'.repeat(64) }, plan: plan({ kind: 'delete' }) }), { status: 202 });
		}) as typeof fetch);
		await c.deleteService('a/b', false);
		await c.deleteService('a/b', true);
		expect(calls[0].url).toBe('/api/v1/services/a%2Fb');
		expect(calls[0].init?.body).toBeUndefined();
		expect(calls[1].init?.body).toBe('{"removeVolumes":true}');
	});
	it('rejects a malformed plan as invalid_response', async () => {
		const c = createClient(respond(200, { kind: 'create' }) as unknown as typeof fetch);
		await expect(c.previewService({ swarm: 's', name: 'n', type: 'container', spec: {} })).rejects.toMatchObject({ kind: 'invalid_response' });
	});
});
