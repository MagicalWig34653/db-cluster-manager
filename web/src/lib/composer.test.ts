import { describe, expect, it } from 'vitest';
import catalogJson from './mock-composer-catalog.json';
import { createClient, isComposerCatalog, isComposerPreview, type ComposerCatalog } from './api';
import { composerMock } from './mock-composer';
import { switchComponent, orderedComponents, buildRecipe, defaultForm, featuresOn, issuesAt, localText, platformsOn, reconcile, requiredBy, settingPath, versionsOn, applicableFields } from './composer';

const cat = catalogJson as unknown as ComposerCatalog;
const nginx = cat.components.find((c) => c.id === 'nginx')!;
const php = cat.components.find((c) => c.id === 'php')!;

describe('catalog guard and texts', () => {
	it('accepts the catalog and rejects broken shapes', () => {
		expect(isComposerCatalog(cat)).toBe(true);
		expect(isComposerCatalog({ ...cat, components: [{ id: 'x' }] })).toBe(false);
		expect(isComposerCatalog(null)).toBe(false);
	});
	it('every setting has both languages and the picker follows the locale', () => {
		for (const c of cat.components) for (const f of c.settings ?? []) expect(f.label.en && f.label.de).toBeTruthy();
		expect(localText(nginx.name, 'de')).toBe('nginx-Webserver');
		expect(localText(nginx.name, 'en')).toBe('nginx web server');
		expect(localText(undefined, 'de')).toBe('');
	});
});

describe('choices follow the compatibility matrix', () => {
	it('offers only versions that exist on the release, newest first', () => {
		expect(versionsOn(php, 'alpine', '3.20')).toEqual(['8.3', '8.2']);
		expect(versionsOn(php, 'debian', '12')).toEqual(['8.2']);
		expect(versionsOn(nginx, 'debian', '12')).toEqual(['1.22']);
		expect(versionsOn(nginx, 'almalinux', '9')).toEqual(['1.26', '1.24', '1.22']);
		expect(versionsOn(php, 'debian', '13')).toEqual(['8.4']);
		expect(versionsOn(php, 'ubuntu', '22.04')).toEqual(['8.1']);
		expect(versionsOn(nginx, 'rocky', '9')).toEqual(['1.26', '1.24', '1.22']);
	});
	it('offers extensions per distribution family', () => {
		expect(featuresOn(cat, php, 'alpine')).toContain('redis');
		expect(featuresOn(cat, php, 'almalinux')).not.toContain('redis');
	});
});

describe('reconcile', () => {
	it('moves versions and drops what the new base does not offer', () => {
		let f = defaultForm(cat);
		f.components.php.features = ['redis', 'mbstring'];
		f.components.php.version = '8.3';
		f = reconcile(cat, { ...f, distro: 'debian', release: '12' });
		expect(f.components.php.version).toBe('8.2');
		expect(f.components.nginx.version).toBe('1.22');
		f = reconcile(cat, { ...f, distro: 'almalinux', release: '9' });
		expect(f.components.php.features).toEqual(['mbstring']);
	});
	it('switches off a component the release does not have and what depends on it', () => {
		let f = defaultForm(cat);
		f.components.composer.on = true;
		f = reconcile(cat, { ...f, distro: 'almalinux', release: '9' });
		expect(f.components.composer.on).toBe(false);
	});
	it('drops settings whose precondition vanished', () => {
		let f = defaultForm(cat);
		f.components.php.settings['opcache.enable'] = false;
		f = reconcile(cat, { ...f, components: { ...f.components, php: { ...f.components.php, features: ['mbstring'] } } });
		expect('opcache.enable' in f.components.php.settings).toBe(false);
		f.components.nginx.settings.fastcgiReadTimeout = '30';
		f = reconcile(cat, { ...f, components: { ...f.components, php: { ...f.components.php, on: false } } });
		expect('fastcgiReadTimeout' in f.components.nginx.settings).toBe(false);
	});
	it('limits platforms to what every component offers', () => {
		const f = defaultForm(cat);
		expect(platformsOn(cat, f)).toEqual(['linux/amd64', 'linux/arm64']);
		expect(reconcile(cat, { ...f, platforms: ['linux/s390x'] }).platforms).toEqual(['linux/amd64']);
	});
});

describe('buildRecipe', () => {
	it('sends only components that are on and settings that differ from the default', () => {
		const f = defaultForm(cat);
		f.name = ' shop ';
		f.digest = 'sha256:' + 'a'.repeat(64);
		f.components.php.settings['memory_limit'] = '256M';
		f.components.php.settings['expose_php'] = false; // the default: not sent
		f.components.nginx.settings['workerConnections'] = '2048';
		f.components.nginx.settings['gzip'] = false;
		const r = buildRecipe(cat, f);
		expect(r.name).toBe('shop');
		expect(r.components.map((c) => c.id)).toEqual(['nginx', 'php']);
		expect(r.components[0].settings).toEqual({ workerConnections: 2048, gzip: false });
		expect(r.components[1].settings).toEqual({ memory_limit: '256M' });
		expect(r.components[1].features).toEqual(['mbstring', 'mysqli', 'opcache']);
	});
	it('keeps non-numeric text for int fields so the manager reports it', () => {
		const f = defaultForm(cat);
		f.components.nginx.settings['workerConnections'] = '10; id';
		expect(buildRecipe(cat, f).components[0].settings).toEqual({ workerConnections: '10; id' });
	});
	it('never sends settings that do not apply', () => {
		const f = defaultForm(cat);
		f.components.php.on = false;
		f.components.nginx.settings['fastcgiReadTimeout'] = '90';
		expect(buildRecipe(cat, f).components[0].settings).toBeUndefined();
		expect(applicableFields(nginx, f).some((x) => x.key === 'fastcgiReadTimeout')).toBe(false);
	});
});

describe('helpers', () => {
	it('maps issues to field paths', () => {
		const issues = [{ path: settingPath('php', 'pm'), message: 'x' }, { path: 'base.digest', message: 'y' }];
		expect(issuesAt(issues, 'components[php].settings.pm')).toHaveLength(1);
		expect(issuesAt(issues, 'base')).toHaveLength(1);
		expect(issuesAt(issues, 'components[php]')).toHaveLength(1);
		expect(issuesAt(issues, 'components[nginx]')).toHaveLength(0);
	});
	it('names the components that need another one', () => {
		const f = defaultForm(cat);
		f.components.composer.on = true;
		expect(requiredBy(cat, f, 'php')).toEqual(['composer']);
	});
});

describe('client against the mock server', () => {
	const api = createClient(async (input, init) => {
		const url = new URL(typeof input === 'string' ? input : (input as Request).url, 'http://x');
		return composerMock((init?.method ?? 'GET').toUpperCase(), url.pathname, init?.body ? JSON.parse(String(init.body)) : {}) ?? new Response('{}', { status: 404 });
	});
	const form = () => {
		const f = defaultForm(cat);
		f.name = 'shop';
		f.digest = 'sha256:' + 'a'.repeat(64);
		return f;
	};
	it('reads the catalog and previews a recipe; invalid recipes answer with findings', async () => {
		expect(isComposerCatalog(await api.composerCatalog())).toBe(true);
		const ok = await api.composerPreview(buildRecipe(cat, form()));
		expect(isComposerPreview(ok)).toBe(true);
		expect(ok.valid).toBe(true);
		expect(ok.dockerfile).toContain('MOCK');
		const bad = await api.composerPreview(buildRecipe(cat, { ...form(), digest: '' }));
		expect(bad.valid).toBe(false);
		expect(issuesAt(bad.issues, 'base.digest')).toHaveLength(1);
	});
	it('plans first and builds only with the plan digest', async () => {
		const req = { recipe: buildRecipe(cat, form()), target: { registry: 'main', repository: 'team/shop', tag: '1' } };
		const plan = await api.composerPlan(req);
		expect(plan.plan?.digest).toBeTruthy();
		await expect(api.composerBuild(req, 'sha256:' + '0'.repeat(64))).rejects.toMatchObject({ kind: 'conflict' });
		const built = await api.composerBuild(req, plan.plan!.digest);
		expect(built.job?.status).toBe('succeeded');
	});
});

describe('extended catalog', () => {
	it('offers the new bases, runtimes and storage components', () => {
		for (const [d, r] of [['alpine', '3.21'], ['alpine', '3.22'], ['debian', '13'], ['ubuntu', '22.04'], ['rocky', '9']]) expect(cat.distros.find((x) => x.id === d)?.releases.some((x) => x.id === r)).toBe(true);
		for (const id of ['apache', 'caddy', 'node', 'python', 'java', 'minecraft', 'sftp', 'jobrunner', 'backuptools', 'dbclients', 'system']) expect(cat.components.some((c) => c.id === id)).toBe(true);
		const node = cat.components.find((c) => c.id === 'node')!;
		expect(versionsOn(node, 'ubuntu', '24.04')).toEqual([]);
		expect(versionsOn(node, 'alpine', '3.21')).toEqual(['22']);
	});
	it('every new setting has both languages (labels and help)', () => {
		for (const c of cat.components) for (const f of c.settings ?? []) {
			expect(f.label.en && f.label.de).toBeTruthy();
			if (f.help) expect(f.help.en && f.help.de).toBeTruthy();
		}
	});
	it('a main process replaces another one and PHP only accompanies web servers', () => {
		let f = defaultForm(cat);
		f = switchComponent(cat, f, 'apache', true);
		expect(f.components.nginx.on).toBe(false);
		expect(f.components.apache.on).toBe(true);
		expect(f.components.php.on).toBe(true);
		f = switchComponent(cat, f, 'node', true);
		expect(f.components.apache.on).toBe(false);
		expect(f.components.php.on).toBe(false);
		f = switchComponent(cat, f, 'php', true);
		expect(f.components.node.on).toBe(false);
		f = switchComponent(cat, f, 'system', true);
		expect(f.components.php.on).toBe(true);
	});
	it('groups components by kind and flags the multi-line certificate field', () => {
		const kinds = orderedComponents(cat).map((c) => c.kind);
		expect(kinds).toEqual([...kinds].sort((a, b) => ['server', 'runtime', 'tool', 'utility'].indexOf(a) - ['server', 'runtime', 'tool', 'utility'].indexOf(b)));
		expect(cat.components.find((c) => c.id === 'system')!.settings!.find((f) => f.key === 'extraCaPem')!.multiline).toBe(true);
	});
	it('does not send an empty certificate field', () => {
		let f = defaultForm(cat);
		f = switchComponent(cat, f, 'system', true);
		const r = buildRecipe(cat, f);
		expect(r.components.find((c) => c.id === 'system')!.settings).toBeUndefined();
	});
});
