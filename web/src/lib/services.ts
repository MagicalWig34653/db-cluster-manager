// Pure rules for the service request form (container and Traefik types) and
// for reading a service plan. The manager validates every request again with
// the same rules (internal/service); these checks only keep the form honest
// and explain problems before a plan is requested.

import type { ServiceInput, ServicePlan, ServiceType } from './api';

export const serviceNameRE = /^[a-z][a-z0-9-]{0,30}$/;
const envNameRE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
const volumeNameRE = /^[a-z0-9][a-z0-9_.-]{0,62}$/;
const hostNameRE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/;
const emailRE = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;
/** Names of variables that normally hold secrets; their value must come from a Swarm secret. */
export const secretEnvRE = /(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL)S?$/i;

export type RestartPolicy = 'any' | 'on-failure' | 'none';
export const logLevels = ['DEBUG', 'INFO', 'WARN', 'ERROR'] as const;

export interface RouteRow {
	/** Public host name the router matches. */
	host: string;
	/** Backend URL, for example http://app:8080. */
	backend: string;
	tls: boolean;
}

export interface ServiceForm {
	swarm: string;
	name: string;
	type: ServiceType;
	image: string;
	/** Swarm node ID the service is pinned to; empty means any node. */
	hostId: string;
	// container
	replicas: string;
	env: string;
	volumes: string;
	networks: string;
	secrets: string;
	cpuMilli: string;
	memoryMiB: string;
	restart: RestartPolicy;
	// traefik
	logLevel: (typeof logLevels)[number];
	acmeEmail: string;
	acmeStaging: boolean;
	routes: RouteRow[];
}

export const emptyServiceForm = (type: ServiceType = 'container'): ServiceForm => ({
	swarm: '',
	name: '',
	type,
	image: type === 'traefik' ? 'traefik:v3.1.2' : '',
	hostId: '',
	replicas: '1',
	env: '',
	volumes: '',
	networks: '',
	secrets: '',
	cpuMilli: '',
	memoryMiB: '',
	restart: 'any',
	logLevel: 'INFO',
	acmeEmail: '',
	acmeStaging: true,
	routes: [{ host: '', backend: '', tls: true }]
});

export type ServiceFormError =
	| 'swarm'
	| 'name'
	| 'image'
	| 'replicas'
	| 'env'
	| 'envSecret'
	| 'volumes'
	| 'volumeHost'
	| 'networks'
	| 'cpu'
	| 'memory'
	| 'email'
	| 'routes'
	| 'tlsEmail';

/** A pinned image has a tag other than latest, or a digest. */
export function imagePinned(img: string): boolean {
	const i = img.trim();
	if (i === '' || i.length > 255 || /\s/.test(i)) return false;
	if (i.includes('@sha256:')) return true;
	const last = i.slice(i.lastIndexOf('/') + 1);
	const c = last.lastIndexOf(':');
	return c >= 0 && c < last.length - 1 && last.slice(c + 1) !== 'latest';
}

const lines = (s: string) =>
	s
		.split('\n')
		.map((l) => l.trim())
		.filter((l) => l !== '' && !l.startsWith('#'));
const list = (s: string) =>
	s
		.split(/[,\s]+/)
		.map((x) => x.trim())
		.filter(Boolean);

export interface EnvParse {
	vars: { name: string; value: string }[];
	invalid: boolean;
	/** A variable that looks like a secret was given a literal value. */
	secret: boolean;
}

/** One NAME=value per line. Secret-looking names are flagged: the value must come from a Swarm secret. */
export function parseEnv(text: string): EnvParse {
	const vars: { name: string; value: string }[] = [];
	let invalid = false;
	let secret = false;
	const seen = new Set<string>();
	for (const l of lines(text)) {
		const eq = l.indexOf('=');
		const name = eq < 0 ? l : l.slice(0, eq).trim();
		if (eq < 0 || !envNameRE.test(name) || seen.has(name)) {
			invalid = true;
			continue;
		}
		seen.add(name);
		if (secretEnvRE.test(name) && !name.endsWith('_FILE')) secret = true;
		vars.push({ name, value: l.slice(eq + 1) });
	}
	return { vars, invalid, secret };
}

export interface VolumeParse {
	volumes: { name: string; target: string; readOnly?: boolean }[];
	invalid: boolean;
}

/** One name:/target[:ro] per line; volumes are managed local volumes, never host paths. */
export function parseVolumes(text: string): VolumeParse {
	const volumes: VolumeParse['volumes'] = [];
	let invalid = false;
	const seen = new Set<string>();
	for (const l of lines(text)) {
		const [name, target, mode, ...rest] = l.split(':');
		const ok = name && volumeNameRE.test(name) && target?.startsWith('/') && !target.includes('..') && rest.length === 0 && (mode === undefined || mode === 'ro') && !seen.has(name);
		if (!ok) {
			invalid = true;
			continue;
		}
		seen.add(name);
		volumes.push({ name, target, ...(mode === 'ro' ? { readOnly: true } : {}) });
	}
	return { volumes, invalid };
}

const wholeNumber = (s: string, min: number, max: number) => /^\d+$/.test(s.trim()) && Number(s) >= min && Number(s) <= max;

export function routeName(host: string): string {
	return host.trim().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
}

const backendURL = (s: string) => {
	try {
		const u = new URL(s.trim());
		return (u.protocol === 'http:' || u.protocol === 'https:') && u.host !== '';
	} catch {
		return false;
	}
};

export function validateServiceForm(f: ServiceForm): ServiceFormError[] {
	const out: ServiceFormError[] = [];
	if (f.swarm === '') out.push('swarm');
	if (!serviceNameRE.test(f.name)) out.push('name');
	if (!imagePinned(f.image)) out.push('image');
	if (f.type === 'container') {
		if (!wholeNumber(f.replicas, 0, 50)) out.push('replicas');
		const env = parseEnv(f.env);
		if (env.invalid) out.push('env');
		if (env.secret) out.push('envSecret');
		const vol = parseVolumes(f.volumes);
		if (vol.invalid) out.push('volumes');
		if (vol.volumes.length > 0 && (f.hostId === '' || Number(f.replicas) > 1)) out.push('volumeHost');
		if (list(f.networks).some((n) => !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(n))) out.push('networks');
		if (f.cpuMilli.trim() !== '' && !wholeNumber(f.cpuMilli, 1, 1_000_000)) out.push('cpu');
		if (f.memoryMiB.trim() !== '' && !wholeNumber(f.memoryMiB, 4, 1_048_576)) out.push('memory');
	} else {
		const rows = f.routes.filter((r) => r.host.trim() !== '' || r.backend.trim() !== '');
		if (rows.some((r) => !hostNameRE.test(r.host.trim()) || !backendURL(r.backend))) out.push('routes');
		if (f.acmeEmail.trim() !== '' && !emailRE.test(f.acmeEmail.trim())) out.push('email');
		if (rows.some((r) => r.tls) && f.acmeEmail.trim() === '') out.push('tlsEmail');
	}
	return out;
}

const resources = (f: ServiceForm) => {
	const limits: Record<string, number> = {};
	if (f.cpuMilli.trim() !== '') limits.milli_cpu = Number(f.cpuMilli);
	if (f.memoryMiB.trim() !== '') limits.memory_bytes = Number(f.memoryMiB) * 1024 * 1024;
	return { limits, reservations: {} };
};

/** The request body for the plan preview and for the create call. Carries no secret values. */
export function buildServiceInput(f: ServiceForm): ServiceInput {
	const placement = f.hostId ? { hostId: f.hostId } : {};
	if (f.type === 'container') {
		const env = parseEnv(f.env).vars;
		const vols = parseVolumes(f.volumes).volumes;
		const networks = list(f.networks);
		const secrets = list(f.secrets);
		return {
			swarm: f.swarm,
			name: f.name,
			type: 'container',
			spec: {
				image: f.image.trim(),
				replicas: Number(f.replicas),
				...(env.length ? { env } : {}),
				...(vols.length ? { volumes: vols } : {}),
				...(networks.length ? { networks } : {}),
				...(secrets.length ? { secrets } : {}),
				resources: resources(f),
				placement,
				restart: f.restart
			}
		};
	}
	const rows = f.routes.filter((r) => r.host.trim() !== '' && r.backend.trim() !== '');
	const tls = rows.some((r) => r.tls) && f.acmeEmail.trim() !== '';
	return {
		swarm: f.swarm,
		name: f.name,
		type: 'traefik',
		spec: {
			image: f.image.trim(),
			placement,
			resources: { limits: {}, reservations: {} },
			logLevel: f.logLevel,
			entryPoints: [
				{ name: 'web', port: 80, published: 80, ...(tls ? { redirectTo: 'websecure' } : {}) },
				...(tls ? [{ name: 'websecure', port: 443, published: 443 }] : [])
			],
			...(tls
				? {
						certResolvers: [
							{ name: 'letsencrypt', email: f.acmeEmail.trim(), staging: f.acmeStaging, challenge: { type: 'http', entryPoint: 'web' } }
						]
					}
				: {}),
			routers: rows.map((r) => ({
				name: routeName(r.host),
				match: { hosts: [r.host.trim()] },
				entryPoints: [r.tls && tls ? 'websecure' : 'web'],
				service: routeName(r.host),
				...(r.tls && tls ? { tls: { certResolver: 'letsencrypt' } } : {})
			})),
			services: rows.map((r) => ({ name: routeName(r.host), servers: [r.backend.trim()] }))
		}
	};
}

// ---------------------------------------------------------------------------
// Plans

export interface PlanView {
	/** A plan can be started only without blockers and when the manager reports it executable. */
	canStart: boolean;
	blocked: boolean;
	/** The number of generated artifacts and steps, for the headline. */
	steps: number;
	artifacts: number;
}

export function planView(p: ServicePlan): PlanView {
	const blocked = p.blockers.length > 0 || !p.executable;
	return { canStart: !blocked, blocked, steps: p.steps.length, artifacts: p.artifacts.length };
}

/** First 12 characters of a digest for tables; the full value is always available on request. */
export const shortDigest = (d: string): string => d.replace(/^sha256:/, '').slice(0, 12);
