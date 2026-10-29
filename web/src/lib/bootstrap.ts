// Fresh-server bootstrap (X18, ADR 0027): types, response guards and the form
// model of the wizard. Secrets live only in the form state and are cleared
// after every request.

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const isStr = (v: unknown): v is string => typeof v === 'string';
const isBool = (v: unknown): v is boolean => typeof v === 'boolean';

export type BootstrapStatus =
	| 'awaiting-fingerprint' | 'planned' | 'blocked' | 'approved' | 'running' | 'paused' | 'main-complete' | 'completed' | 'cancelled';
const statuses: BootstrapStatus[] = ['awaiting-fingerprint', 'planned', 'blocked', 'approved', 'running', 'paused', 'main-complete', 'completed', 'cancelled'];

export interface BootstrapCluster {
	platform: 'none' | 'swarm' | 'k3s';
	role?: 'init' | 'join-manager' | 'join-worker';
	managerAddress?: string;
}
export interface BootstrapProfile {
	serviceUser: string;
	firewall: boolean;
	unattendedUpgrades: boolean;
	dedicatedKey: boolean;
	disableRootLogin: boolean;
	runtime: 'docker' | 'none';
	nativeAgent: boolean;
	containerAgent: boolean;
	patch: boolean;
	patchApply: boolean;
	restic: boolean;
	resticPaths?: string[];
	cluster: BootstrapCluster;
}
export interface BootstrapFinding { severity: 'block' | 'warn' | 'info'; code: string; message: string }
export interface BootstrapStep {
	id: string;
	title: string;
	phase: string;
	mutating: boolean;
	runAs: string;
	script?: string;
	scriptDigest?: string;
	check?: string;
	cleanup?: string;
	stdin?: string;
	managerAction?: string;
	gate?: string;
	expect: 'done' | 'todo';
}
export interface BootstrapPlan {
	id: string;
	host: { id: string; address: string; user: string };
	profile: BootstrapProfile;
	hostKeyFingerprint?: string;
	os: { id: string; name: string; packageManager: string };
	findings: BootstrapFinding[];
	blocked: boolean;
	steps: BootstrapStep[];
	dedicatedPublicKey?: string;
	digest: string;
	lockdownDigest?: string;
}
export interface BootstrapProgressStep { id: string; title: string; state: string }
export interface BootstrapView {
	id: string;
	status: BootstrapStatus;
	host: { id: string; address: string; user: string };
	hostKey?: { type: string; fingerprint: string };
	plan?: BootstrapPlan;
	progress: BootstrapProgressStep[];
	error?: string;
	mainApproved: boolean;
	lockdownApproved: boolean;
	credentialsHeld: boolean;
}
export interface BootstrapOS {
	id: string;
	name: string;
	versions: string[];
	packageManager: string;
	init: string;
	docker: string;
	nativeAgent: boolean;
	firewall?: string;
	unattended?: string;
}

const isFinding = (v: unknown): v is BootstrapFinding => isObj(v) && ['block', 'warn', 'info'].includes(v.severity as string) && isStr(v.code) && isStr(v.message);
const isStep = (v: unknown): v is BootstrapStep => isObj(v) && isStr(v.id) && isStr(v.title) && isStr(v.phase) && isBool(v.mutating) && (v.expect === 'done' || v.expect === 'todo');
export const isBootstrapPlan = (v: unknown): v is BootstrapPlan =>
	isObj(v) && isStr(v.id) && isObj(v.profile) && isBool(v.blocked) && Array.isArray(v.findings) && v.findings.every(isFinding) &&
	Array.isArray(v.steps) && v.steps.every(isStep) && isStr(v.digest) && /^[a-f0-9]{64}$/.test(v.digest) &&
	!('credential' in v) && !('privateKey' in v) && !('password' in v);
export const isBootstrapView = (v: unknown): v is BootstrapView =>
	isObj(v) && isStr(v.id) && statuses.includes(v.status as BootstrapStatus) && isObj(v.host) &&
	Array.isArray(v.progress) && v.progress.every((p) => isObj(p) && isStr(p.id) && isStr(p.state)) &&
	isBool(v.mainApproved) && isBool(v.lockdownApproved) && isBool(v.credentialsHeld) &&
	(v.hostKey === undefined || (isObj(v.hostKey) && isStr(v.hostKey.fingerprint))) &&
	(v.plan === undefined || isBootstrapPlan(v.plan)) && !('credential' in v) && !('password' in v);
export const isBootstrapViewList = (v: unknown): v is BootstrapView[] => Array.isArray(v) && v.every(isBootstrapView);
export const isBootstrapOSList = (v: unknown): v is BootstrapOS[] =>
	Array.isArray(v) && v.every((o) => isObj(o) && isStr(o.id) && isStr(o.name) && Array.isArray(o.versions) && isStr(o.packageManager));

export interface BootstrapForm {
	id: string;
	address: string;
	user: string;
	authKind: 'password' | 'key';
	password: string;
	privateKey: string;
	profile: BootstrapProfile;
	joinToken: string;
	resticPathsText: string;
}

export function defaultBootstrapForm(): BootstrapForm {
	return {
		id: '', address: '', user: 'root', authKind: 'password', password: '', privateKey: '', joinToken: '', resticPathsText: '',
		profile: {
			serviceUser: 'dbcm', firewall: false, unattendedUpgrades: false, dedicatedKey: false, disableRootLogin: false,
			runtime: 'docker', nativeAgent: false, containerAgent: false, patch: false, patchApply: false, restic: false,
			cluster: { platform: 'none' }
		}
	};
}

const name = /^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$/;
const svcUser = /^[a-z_][a-z0-9_-]{0,30}$/;

export function parsePaths(text: string): string[] {
	return text.split('\n').map((l) => l.trim()).filter(Boolean);
}

/** Returns message keys of everything wrong with the form; empty when valid. Mirrors BootstrapProfile.Validate in Go. */
export function bootstrapProblems(f: BootstrapForm): string[] {
	const p = f.profile;
	const out: string[] = [];
	if (!name.test(f.id)) out.push('bootProbId');
	if (!name.test(f.address) || !name.test(f.user)) out.push('bootProbHost');
	if (f.authKind === 'password' ? !f.password : !f.privateKey.trim()) out.push('bootProbCredential');
	if (!svcUser.test(p.serviceUser) || p.serviceUser === 'root') out.push('bootProbServiceUser');
	if (p.cluster.platform === 'swarm' && p.runtime !== 'docker') out.push('bootProbSwarmRuntime');
	if (p.cluster.platform === 'k3s' && p.runtime !== 'none') out.push('bootProbK3sRuntime');
	if (p.cluster.platform !== 'none') {
		if (!p.cluster.role) out.push('bootProbClusterRole');
		else if (p.cluster.role !== 'init' && !/^[A-Za-z0-9][A-Za-z0-9.-]{0,252}:[0-9]{2,5}$/.test(p.cluster.managerAddress ?? '')) out.push('bootProbManagerAddress');
		if (p.cluster.role && p.cluster.role !== 'init' && p.cluster.platform === 'swarm' && !f.joinToken.trim()) out.push('bootProbJoinToken');
	}
	if (p.containerAgent && p.cluster.platform !== 'swarm') out.push('bootProbContainerAgent');
	if ((p.patch || p.restic) && !p.nativeAgent) out.push('bootProbNativeAgent');
	if (p.restic && parsePaths(f.resticPathsText).length === 0) out.push('bootProbResticPaths');
	if (p.disableRootLogin && !p.dedicatedKey) out.push('bootProbDedicatedKey');
	return out;
}

/** The request body; secrets are included here and only here. */
export function draftBody(f: BootstrapForm) {
	const p = { ...f.profile, cluster: { ...f.profile.cluster }, resticPaths: f.profile.restic ? parsePaths(f.resticPathsText) : undefined };
	if (p.cluster.platform === 'none') p.cluster = { platform: 'none' };
	return {
		id: f.id,
		host: { id: f.id, address: f.address, user: f.user, role: 'worker' },
		profile: p,
		credential: f.authKind === 'password' ? { password: f.password } : { privateKey: f.privateKey },
		...(f.joinToken.trim() ? { joinToken: f.joinToken.trim() } : {})
	};
}

/** Clears every secret field of the form. */
export function wipeSecrets(f: BootstrapForm) {
	f.password = '';
	f.privateKey = '';
	f.joinToken = '';
}

export type StepState = 'pending' | 'running' | 'succeeded' | 'failed' | 'unknown' | 'aborted';
export function progressPercent(steps: BootstrapProgressStep[]): number {
	if (steps.length === 0) return 0;
	return Math.round((steps.filter((s) => s.state === 'succeeded').length / steps.length) * 100);
}
