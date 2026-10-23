// Pure rules for the web server image composer. The version catalog mirrors
// the manager's proposal catalog (internal/imagebuild/catalog.go); it was
// written from knowledge, not verified against live repositories, and the
// manager decides. The checks here only keep the form honest and explain
// problems before a plan is requested.

import type { ImageBuildRequest, ImageBuildJob, Registry } from './api';

export const nginxVersions = ['1.26', '1.27'] as const;
export const phpVersions = ['8.2', '8.3', '8.4'] as const;
const allExtensions = ['bcmath', 'curl', 'exif', 'gd', 'gmp', 'intl', 'mbstring', 'mysqli', 'opcache', 'pdo_mysql', 'pdo_pgsql', 'pgsql', 'redis', 'sodium', 'xml', 'zip'];
const missing: Record<string, string[]> = { '8.4': ['redis'], '8.2': ['gmp'] };
export const packageCatalog = ['ca-certificates', 'curl', 'ghostscript', 'imagemagick', 'msmtp', 'tzdata'] as const;
export const platformChoices = ['linux/amd64', 'linux/arm64'] as const;

/** Extensions selectable for a PHP version; empty without PHP. */
export const extensionsFor = (php: string): string[] => (php === '' ? [] : allExtensions.filter((e) => !(missing[php] ?? []).includes(e)));

/** Keeps only the extensions that exist for the chosen PHP version (changing the version can drop some). */
export const pruneExtensions = (php: string, selected: readonly string[]): string[] => {
	const ok = extensionsFor(php);
	return selected.filter((e) => ok.includes(e));
};

export interface ImageForm {
	name: string;
	nginxVersion: string;
	/** sha256:<64 hex>; the base image must be pinned by digest. */
	baseDigest: string;
	/** Empty: no PHP. */
	phpVersion: string;
	extensions: string[];
	/** key=value per line. */
	ini: string;
	workerProcesses: string;
	workerConnections: string;
	serverName: string;
	documentRoot: string;
	clientMaxBodySize: string;
	keepaliveTimeout: string;
	fastcgiReadTimeout: string;
	gzip: boolean;
	serverTokens: boolean;
	securityHeaders: boolean;
	snippet: string;
	packages: string[];
	platforms: string[];
	registry: string;
	repository: string;
	tag: string;
}

export const emptyImageForm = (): ImageForm => ({
	name: '',
	nginxVersion: '1.27',
	baseDigest: '',
	phpVersion: '8.3',
	extensions: ['mysqli', 'mbstring', 'opcache'],
	ini: 'memory_limit=256M\nupload_max_filesize=64M',
	workerProcesses: 'auto',
	workerConnections: '1024',
	serverName: '_',
	documentRoot: '/var/www/html',
	clientMaxBodySize: '64M',
	keepaliveTimeout: '65',
	fastcgiReadTimeout: '60',
	gzip: true,
	serverTokens: false,
	securityHeaders: true,
	snippet: '',
	packages: [],
	platforms: ['linux/amd64'],
	registry: '',
	repository: '',
	tag: ''
});

export const digestRE = /^sha256:[0-9a-f]{64}$/;
const nameRE = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$/;
const sizeRE = /^[1-9][0-9]{0,5}[kKmMgG]?$/;
const hostRE = /^(_|[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?)$/;
const rootRE = /^\/var\/www(\/[A-Za-z0-9._-]+)*$/;
const repoRE = /^[a-z0-9]+([._-][a-z0-9]+)*(\/[a-z0-9]+([._-][a-z0-9]+)*)*$/;
const tagRE = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;
const iniKeyRE = /^[a-z][a-z0-9_.]{0,63}$/;

export const MAX_SNIPPET_BYTES = 8192;
export const MAX_SNIPPET_LINES = 200;
/** Directives the manager refuses in a snippet (deny-list; the build's nginx -t is the final check). */
const deniedDirectives = new Set([
	'load_module', 'include', 'user', 'pid', 'daemon', 'env', 'working_directory', 'error_log', 'access_log', 'ssl_certificate_key',
	'ssl_password_file', 'auth_basic_user_file', 'master_process', 'worker_rlimit_core', 'lock_file', 'proxy_ssl_certificate_key', 'uwsgi_pass', 'scgi_pass'
]);
const deniedPrefixes = ['lua_', 'perl', 'js_', 'njs_', 'set_by_lua', 'content_by_lua', 'access_by_lua', 'rewrite_by_lua', 'header_filter_by_lua', 'body_filter_by_lua', 'log_by_lua', 'init_by_lua', 'init_worker_by_lua', 'ssl_certificate_by_lua'];

export type SnippetProblem = 'size' | 'lines' | { directive: string };

/** Early hints only: the manager repeats the check. Returns the problems found. */
export function snippetProblems(s: string): SnippetProblem[] {
	const out: SnippetProblem[] = [];
	if (new TextEncoder().encode(s).length > MAX_SNIPPET_BYTES) out.push('size');
	if (s.split('\n').length > MAX_SNIPPET_LINES) out.push('lines');
	const seen = new Set<string>();
	for (const stmt of s.split(/[;{}\n]/)) {
		const word = stmt.trim().split(/\s+/)[0]?.toLowerCase() ?? '';
		if (word === '' || word.startsWith('#') || seen.has(word)) continue;
		if (deniedDirectives.has(word) || deniedPrefixes.some((p) => word.startsWith(p))) {
			seen.add(word);
			out.push({ directive: word });
		}
	}
	return out;
}

export interface IniParse {
	ini: Record<string, string>;
	invalid: boolean;
}

export function parseIni(text: string): IniParse {
	const ini: Record<string, string> = {};
	let invalid = false;
	for (const raw of text.split('\n')) {
		const l = raw.trim();
		if (l === '' || l.startsWith(';') || l.startsWith('#')) continue;
		const eq = l.indexOf('=');
		const k = eq < 0 ? '' : l.slice(0, eq).trim();
		if (!iniKeyRE.test(k) || k in ini) {
			invalid = true;
			continue;
		}
		ini[k] = l.slice(eq + 1).trim();
	}
	return { ini, invalid };
}

export type ImageFormError =
	| 'name'
	| 'digest'
	| 'ini'
	| 'iniWithoutPhp'
	| 'workerProcesses'
	| 'workerConnections'
	| 'serverName'
	| 'documentRoot'
	| 'bodySize'
	| 'keepalive'
	| 'fastcgi'
	| 'snippet'
	| 'registry'
	| 'repository'
	| 'tag';

const intIn = (s: string, lo: number, hi: number) => /^\d+$/.test(s.trim()) && Number(s) >= lo && Number(s) <= hi;

export function validateImageForm(f: ImageForm): ImageFormError[] {
	const out: ImageFormError[] = [];
	if (!nameRE.test(f.name.trim())) out.push('name');
	if (!digestRE.test(f.baseDigest.trim())) out.push('digest');
	const ini = parseIni(f.ini);
	if (ini.invalid) out.push('ini');
	if (f.phpVersion === '' && (Object.keys(ini.ini).length > 0 || f.extensions.length > 0)) out.push('iniWithoutPhp');
	if (f.workerProcesses !== 'auto' && !intIn(f.workerProcesses, 1, 64)) out.push('workerProcesses');
	if (!intIn(f.workerConnections, 64, 65535)) out.push('workerConnections');
	if (!hostRE.test(f.serverName.trim())) out.push('serverName');
	if (!rootRE.test(f.documentRoot.trim())) out.push('documentRoot');
	if (!sizeRE.test(f.clientMaxBodySize.trim())) out.push('bodySize');
	if (!intIn(f.keepaliveTimeout, 0, 600)) out.push('keepalive');
	if (f.phpVersion !== '' && !intIn(f.fastcgiReadTimeout, 1, 3600)) out.push('fastcgi');
	if (snippetProblems(f.snippet).length > 0) out.push('snippet');
	if (f.registry === '') out.push('registry');
	if (!repoRE.test(f.repository.trim())) out.push('repository');
	if (!tagRE.test(f.tag.trim())) out.push('tag');
	return out;
}

/** The request for the plan preview. The approval digest is added only by the start call. */
export function buildImageRequest(f: ImageForm): ImageBuildRequest {
	const snippet = f.snippet.trim();
	return {
		spec: {
			name: f.name.trim(),
			nginx: {
				version: f.nginxVersion,
				baseDigest: f.baseDigest.trim(),
				options: {
					workerProcesses: f.workerProcesses,
					workerConnections: Number(f.workerConnections),
					serverName: f.serverName.trim(),
					documentRoot: f.documentRoot.trim(),
					clientMaxBodySize: f.clientMaxBodySize.trim(),
					keepaliveTimeout: Number(f.keepaliveTimeout),
					gzip: f.gzip,
					serverTokens: f.serverTokens,
					securityHeaders: f.securityHeaders,
					...(f.phpVersion !== '' ? { fastcgiReadTimeout: Number(f.fastcgiReadTimeout) } : {})
				},
				...(snippet ? { customSnippet: snippet } : {})
			},
			...(f.phpVersion !== ''
				? { php: { version: f.phpVersion, extensions: pruneExtensions(f.phpVersion, f.extensions), ini: parseIni(f.ini).ini } }
				: {}),
			...(f.packages.length ? { extraPackages: [...f.packages] } : {}),
			platforms: f.platforms.length ? [...f.platforms] : ['linux/amd64']
		},
		target: { registry: f.registry, repository: f.repository.trim(), tag: f.tag.trim() }
	};
}

/** Registries a build may push to: all except unreachable placeholders; managed ones are specs only and cannot be registered here. */
export const pushableRegistries = (rs: readonly Registry[]): Registry[] => rs.filter((r) => r.kind !== 'managed');

/** The plan shown to the user is the one that gets approved: any edit invalidates it. */
export const planStillValid = (shown: string | undefined, current: string): boolean => shown !== undefined && shown === current;

export const buildOrder = (jobs: readonly ImageBuildJob[]): ImageBuildJob[] =>
	[...jobs].sort((a, b) => (Date.parse(b.createdAt ?? '') || 0) - (Date.parse(a.createdAt ?? '') || 0));

// ---------------------------------------------------------------------------
// Registry form

const registryIdRE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/;

export interface RegistryForm {
	id: string;
	name: string;
	kind: 'ghcr' | 'dockerhub' | 'generic';
	endpoint: string;
	username: string;
	password: string;
}

export const emptyRegistryForm = (): RegistryForm => ({ id: '', name: '', kind: 'ghcr', endpoint: '', username: '', password: '' });

export type RegistryFormError = 'id' | 'name' | 'endpoint' | 'username';

export function validateRegistryForm(f: RegistryForm): RegistryFormError[] {
	const out: RegistryFormError[] = [];
	if (!registryIdRE.test(f.id)) out.push('id');
	if (f.name.trim() === '' || f.name.length > 100) out.push('name');
	if (f.kind === 'generic') {
		try {
			const u = new URL(f.endpoint.trim());
			const loop = ['localhost', '127.0.0.1', '[::1]'].includes(u.hostname);
			if (!(u.protocol === 'https:' || (u.protocol === 'http:' && loop)) || u.host === '') out.push('endpoint');
		} catch {
			out.push('endpoint');
		}
	}
	if (f.username.length > 512) out.push('username');
	return out;
}

/** The password is sent only when typed, exactly once; the form is cleared by the caller afterwards. */
export function buildRegistryInput(f: RegistryForm) {
	return {
		id: f.id,
		name: f.name.trim(),
		kind: f.kind,
		...(f.kind === 'generic' ? { endpoint: f.endpoint.trim() } : {}),
		...(f.username.trim() ? { username: f.username.trim() } : {}),
		...(f.password ? { password: f.password } : {})
	};
}
