import { describe, expect, it } from 'vitest';
import { createClient, isImageBuildJob, isImageBuildResult, isRegistry } from './api';
import {
	buildImageRequest,
	buildOrder,
	buildRegistryInput,
	emptyImageForm,
	emptyRegistryForm,
	extensionsFor,
	parseIni,
	planStillValid,
	pruneExtensions,
	pushableRegistries,
	snippetProblems,
	validateImageForm,
	validateRegistryForm
} from './images';

const good = () => ({ ...emptyImageForm(), name: 'Shop web', baseDigest: 'sha256:' + 'a'.repeat(64), registry: 'ghcr-main', repository: 'team/shop-web', tag: '1.0.0' });

describe('catalog', () => {
	it('lists extensions per PHP version and prunes unavailable ones', () => {
		expect(extensionsFor('')).toEqual([]);
		expect(extensionsFor('8.3')).toContain('redis');
		expect(extensionsFor('8.4')).not.toContain('redis');
		expect(extensionsFor('8.2')).not.toContain('gmp');
		expect(pruneExtensions('8.4', ['redis', 'gd'])).toEqual(['gd']);
		expect(pruneExtensions('', ['gd'])).toEqual([]);
	});
});

describe('php.ini parsing', () => {
	it('parses key=value lines and skips comments', () => {
		expect(parseIni('; c\nmemory_limit = 256M\n\nupload_max_filesize=64M').ini).toEqual({ memory_limit: '256M', upload_max_filesize: '64M' });
		expect(parseIni('memory_limit').invalid).toBe(true);
		expect(parseIni('Bad Key=1').invalid).toBe(true);
		expect(parseIni('a=1\na=2').invalid).toBe(true);
	});
});

describe('custom snippet', () => {
	it('accepts plain location blocks and flags dangerous directives', () => {
		expect(snippetProblems('location /health { return 200; }')).toEqual([]);
		expect(snippetProblems('include /etc/passwd;')).toEqual([{ directive: 'include' }]);
		expect(snippetProblems('location / { access_log off; }\nload_module x;')).toEqual([{ directive: 'access_log' }, { directive: 'load_module' }]);
		expect(snippetProblems('content_by_lua_block { }')).toEqual([{ directive: 'content_by_lua_block' }]);
	});
	it('enforces the size and line limits', () => {
		expect(snippetProblems('#'.repeat(8193))).toContain('size');
		expect(snippetProblems('# x\n'.repeat(201))).toContain('lines');
	});
});

describe('image form', () => {
	it('accepts the defaults with a digest and a target', () => {
		expect(validateImageForm(good())).toEqual([]);
	});
	it('requires the base image digest, a name and a target', () => {
		expect(validateImageForm({ ...good(), baseDigest: 'nginx:1.27' })).toEqual(['digest']);
		expect(validateImageForm({ ...good(), name: ' ' })).toEqual(['name']);
		expect(validateImageForm({ ...good(), registry: '', repository: 'Bad Repo', tag: '' })).toEqual(['registry', 'repository', 'tag']);
	});
	it('checks nginx options and PHP settings', () => {
		expect(validateImageForm({ ...good(), workerProcesses: '0', workerConnections: '10', documentRoot: '/etc', clientMaxBodySize: '0m' })).toEqual([
			'workerProcesses',
			'workerConnections',
			'documentRoot',
			'bodySize'
		]);
		expect(validateImageForm({ ...good(), phpVersion: '', ini: '', extensions: ['gd'] })).toEqual(['iniWithoutPhp']);
		expect(validateImageForm({ ...good(), phpVersion: '', ini: '', extensions: [] })).toEqual([]);
		expect(validateImageForm({ ...good(), snippet: 'user root;' })).toEqual(['snippet']);
	});
	it('builds the request: PHP block only with a version, packages only when chosen', () => {
		const r = buildImageRequest({ ...good(), extensions: ['gd', 'redis'], packages: ['curl'], snippet: ' location /x { return 204; } ' });
		expect(r.approvalDigest).toBeUndefined();
		expect(r.spec.nginx.version).toBe('1.27');
		expect(r.spec.nginx.customSnippet).toBe('location /x { return 204; }');
		expect(r.spec.nginx.options).toMatchObject({ workerProcesses: 'auto', workerConnections: 1024, gzip: true, serverTokens: false, fastcgiReadTimeout: 60 });
		expect(r.spec.php).toEqual({ version: '8.3', extensions: ['gd', 'redis'], ini: { memory_limit: '256M', upload_max_filesize: '64M' } });
		expect(r.spec.extraPackages).toEqual(['curl']);
		expect(r.target).toEqual({ registry: 'ghcr-main', repository: 'team/shop-web', tag: '1.0.0' });
		const plain = buildImageRequest({ ...good(), phpVersion: '', extensions: [], ini: '' });
		expect(plain.spec).not.toHaveProperty('php');
		expect(plain.spec).not.toHaveProperty('extraPackages');
		expect(plain.spec.nginx.options).not.toHaveProperty('fastcgiReadTimeout');
		expect(plain.spec.platforms).toEqual(['linux/amd64']);
	});
	it('drops extensions the chosen PHP version does not have', () => {
		expect(buildImageRequest({ ...good(), phpVersion: '8.4', extensions: ['redis', 'gd'] }).spec.php?.extensions).toEqual(['gd']);
	});
	it('treats a plan as approved only while the request is unchanged', () => {
		const a = JSON.stringify(buildImageRequest(good()));
		const b = JSON.stringify(buildImageRequest({ ...good(), tag: '1.0.1' }));
		expect(planStillValid(a, a)).toBe(true);
		expect(planStillValid(a, b)).toBe(false);
		expect(planStillValid(undefined, a)).toBe(false);
	});
});

describe('registry form', () => {
	const f = { ...emptyRegistryForm(), id: 'ghcr-main', name: 'GitHub', username: 'ci', password: 'only-typed' };
	it('validates id, name and the endpoint of generic registries', () => {
		expect(validateRegistryForm(f)).toEqual([]);
		expect(validateRegistryForm({ ...f, id: '-x', name: '' })).toEqual(['id', 'name']);
		expect(validateRegistryForm({ ...f, kind: 'generic', endpoint: '' })).toEqual(['endpoint']);
		expect(validateRegistryForm({ ...f, kind: 'generic', endpoint: 'http://registry.example.org' })).toEqual(['endpoint']);
		expect(validateRegistryForm({ ...f, kind: 'generic', endpoint: 'http://localhost:5000' })).toEqual([]);
		expect(validateRegistryForm({ ...f, kind: 'generic', endpoint: 'https://registry.example.org' })).toEqual([]);
	});
	it('sends the password only when typed and the endpoint only for generic', () => {
		expect(buildRegistryInput(f)).toEqual({ id: 'ghcr-main', name: 'GitHub', kind: 'ghcr', username: 'ci', password: 'only-typed' });
		expect(buildRegistryInput({ ...f, password: '', username: '' })).toEqual({ id: 'ghcr-main', name: 'GitHub', kind: 'ghcr' });
		expect(buildRegistryInput({ ...f, kind: 'generic', endpoint: ' https://r.example.org ' })).toMatchObject({ endpoint: 'https://r.example.org' });
	});
	it('never lets a registry response carry a password into the guard', () => {
		const r = { id: 'r', name: 'n', kind: 'ghcr', credentialSet: true };
		expect(isRegistry(r)).toBe(true);
		expect(isRegistry({ ...r, credentialSet: 'yes' })).toBe(false);
		expect(isRegistry({ ...r, kind: 'quay' })).toBe(false);
	});
	it('hides managed registries from push targets', () => {
		const rs = [r('a', 'ghcr'), r('b', 'managed')];
		expect(pushableRegistries(rs).map((x) => x.id)).toEqual(['a']);
	});
});

function r(id: string, kind: 'ghcr' | 'managed') {
	return { id, name: id, kind, credentialSet: false };
}

describe('build jobs', () => {
	const job = { id: 'b1', status: 'succeeded', specHash: 's', contextHash: 'c', planDigest: 'p', reference: 'ghcr.io/x/y:1', sbom: { status: 'not_generated' }, createdAt: '2026-10-23T10:00:00Z' };
	it('guards jobs and results', () => {
		expect(isImageBuildJob(job)).toBe(true);
		expect(isImageBuildJob({ ...job, status: 'done' })).toBe(false);
		expect(isImageBuildJob({ ...job, sbom: undefined })).toBe(false);
		expect(isImageBuildResult({ job })).toBe(true);
		expect(isImageBuildResult({ plan: { digest: 'd' } })).toBe(false);
	});
	it('orders newest first', () => {
		const older = { ...job, id: 'b0', status: 'failed' as const, createdAt: '2026-10-22T10:00:00Z' };
		expect(buildOrder([older, { ...job, status: 'succeeded' as const }]).map((j) => j.id)).toEqual(['b1', 'b0']);
	});
	it('lists builds as a plain array and previews without an approval digest', async () => {
		const bodies: unknown[] = [];
		const c = createClient((async (url: string, init?: RequestInit) => {
			if (init?.method === 'POST') bodies.push(JSON.parse(String(init.body)));
			return new Response(JSON.stringify(init?.method === 'POST' ? { job } : [job]), { status: init?.method === 'POST' ? 202 : 200 });
		}) as typeof fetch);
		expect(await c.imageBuilds()).toHaveLength(1);
		const req = buildImageRequest(good());
		await c.planImageBuild(req);
		await c.startImageBuild(req, 'dg');
		expect(bodies[0]).not.toHaveProperty('approvalDigest');
		expect(bodies[1]).toMatchObject({ approvalDigest: 'dg' });
	});
});
