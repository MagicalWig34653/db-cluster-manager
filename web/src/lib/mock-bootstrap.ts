// Development-only fixtures of the bootstrap endpoints, loaded through mock.ts
// (import.meta.env.DEV && VITE_MOCK=1). It imitates the shape of the answers
// only: no script below is the manager's generator output, and no host is
// contacted. Credentials are discarded on receipt.

import type { BootstrapPlan, BootstrapProfile, BootstrapStep, BootstrapView } from './bootstrap';

const MOCK_FINGERPRINT = 'SHA256:MOCKmockMOCKmockMOCKmockMOCKmockMOCKmockMOC';
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const digestOf = (s: string) => {
	let h = 7;
	for (const c of s) h = (h * 33 + c.charCodeAt(0)) >>> 0;
	return h.toString(16).padStart(8, '0').repeat(8);
};

interface Draft { view: BootstrapView; ticks: number }
const drafts = new Map<string, Draft>();

const support = [
	{ id: 'debian', name: 'Debian', versions: ['12', '13'], packageManager: 'apt', init: 'systemd', docker: 'download.docker.com/linux/debian (apt)', nativeAgent: true, firewall: 'ufw', unattended: 'unattended-upgrades' },
	{ id: 'ubuntu', name: 'Ubuntu', versions: ['22.04', '24.04'], packageManager: 'apt', init: 'systemd', docker: 'download.docker.com/linux/ubuntu (apt)', nativeAgent: true, firewall: 'ufw', unattended: 'unattended-upgrades' },
	{ id: 'almalinux', name: 'AlmaLinux', versions: ['9', '10'], packageManager: 'dnf', init: 'systemd', docker: 'download.docker.com/linux/centos (dnf)', nativeAgent: true, firewall: 'firewalld', unattended: 'dnf-automatic' },
	{ id: 'rocky', name: 'Rocky Linux', versions: ['9', '10'], packageManager: 'dnf', init: 'systemd', docker: 'download.docker.com/linux/centos (dnf)', nativeAgent: true, firewall: 'firewalld', unattended: 'dnf-automatic' },
	{ id: 'alpine', name: 'Alpine Linux', versions: ['3.19', '3.20', '3.21', '3.22'], packageManager: 'apk', init: 'openrc', docker: 'Alpine community repository (apk)', nativeAgent: false }
];

function mkStep(id: string, title: string, phase: string, extra: Partial<BootstrapStep> = {}): BootstrapStep {
	const script = extra.managerAction ? undefined : `#!/bin/sh\n# MOCK ${id}\nset -eu\necho 'mock step ${id}'\n`;
	return { id, title, phase, mutating: true, runAs: 'root', script, scriptDigest: digestOf(script ?? id), cleanup: '# MOCK cleanup\n', expect: 'todo', ...extra };
}

function buildPlan(id: string, address: string, user: string, p: BootstrapProfile): BootstrapPlan {
	const steps: BootstrapStep[] = [mkStep('base-packages', 'Install base packages (curl, sudo, certificates)', 'baseline'), mkStep('service-user', `Create the service user ${p.serviceUser} with sudo`, 'baseline'),
		mkStep('sshd-audit', 'Audit sshd settings (read-only, changes nothing)', 'baseline', { mutating: false })];
	if (p.firewall) steps.push(mkStep('firewall', 'Enable the firewall (SSH is allowed first)', 'baseline'));
	if (p.unattendedUpgrades) steps.push(mkStep('unattended-upgrades', 'Enable unattended security updates', 'baseline'));
	if (p.runtime === 'docker') steps.push(mkStep('runtime-docker', 'Install the Docker engine from the pinned official repository', 'runtime'));
	if (p.restic) steps.push(mkStep('restic', 'Install restic', 'capability'));
	if (p.nativeAgent) steps.push(mkStep('agent-native', 'Install the native agent and enroll it', 'agent', { stdin: 'enrollment' }));
	if (p.cluster.platform === 'swarm') steps.push(mkStep('cluster-swarm', `Swarm ${p.cluster.role ?? 'init'}`, 'cluster'));
	if (p.containerAgent) steps.push(mkStep('agent-container', 'Deploy the container agent as a Swarm service (manager action)', 'agent', { managerAction: 'deploy-container-agent', runAs: 'manager' }));
	let lockdownDigest: string | undefined;
	if (p.dedicatedKey) {
		steps.push(mkStep('lockdown-install-key', 'Install the dedicated public key', 'lockdown', { gate: 'lockdown' }),
			mkStep('lockdown-verify-key', 'Verify a login with the dedicated key and sudo (new connection)', 'lockdown', { gate: 'lockdown', runAs: 'dedicated', mutating: false }),
			mkStep('lockdown-store-key', 'Seal the dedicated private key in the manager', 'lockdown', { gate: 'lockdown', managerAction: 'store-dedicated-key', runAs: 'manager' }));
		if (p.disableRootLogin) steps.push(mkStep('lockdown-disable-root', 'Disable SSH root login (automatic revert if confirmation fails)', 'lockdown', { gate: 'lockdown' }),
			mkStep('lockdown-confirm', 'Confirm over a new dedicated-key connection', 'lockdown', { gate: 'lockdown', runAs: 'dedicated' }));
		lockdownDigest = digestOf('lockdown' + id + steps.filter((s) => s.gate).map((s) => s.id).join());
	}
	const findings: BootstrapPlan['findings'] = [{ severity: 'info', code: 'mock', message: 'MOCK data: this plan was not produced by the manager.' }];
	if (p.cluster.platform === 'k3s') findings.push({ severity: 'block', code: 'k3s-disabled', message: 'k3s execution over SSH stays disabled (ADR 0018).' });
	if (p.firewall && p.runtime === 'docker') findings.push({ severity: 'warn', code: 'firewall-docker', message: 'Docker publishes container ports through its own firewall rules, which can bypass ufw and firewalld rules.' });
	const blocked = findings.some((f) => f.severity === 'block');
	const plan: BootstrapPlan = { id, host: { id, address, user }, profile: p, hostKeyFingerprint: MOCK_FINGERPRINT, os: { id: 'debian', name: 'Debian', packageManager: 'apt' }, findings, blocked, steps, dedicatedPublicKey: p.dedicatedKey ? 'ssh-ed25519 AAAAMOCK dbcm-' + id : undefined, digest: '', lockdownDigest };
	plan.digest = digestOf(JSON.stringify(plan));
	return plan;
}

function progressOf(d: Draft): BootstrapView['progress'] {
	const plan = d.view.plan;
	if (!plan) return [];
	const st = d.view.status;
	return plan.steps.map((s, i) => {
		let state = 'pending';
		if (st === 'completed') state = 'succeeded';
		else if (!s.gate && st === 'main-complete') state = 'succeeded';
		else if (st === 'running' && d.view.lockdownApproved) state = !s.gate ? 'succeeded' : 'running';
		else if (st === 'running' && !s.gate) state = i < d.ticks ? 'succeeded' : i === d.ticks ? 'running' : 'pending';
		return { id: s.id, title: s.title, state };
	});
}

function snapshot(d: Draft): BootstrapView {
	d.view.progress = progressOf(d);
	return JSON.parse(JSON.stringify(d.view));
}

export function bootstrapMock(method: string, path: string, body: any): Response | undefined {
	const base = '/api/v1/bootstrap/';
	if (!path.startsWith(base)) return undefined;
	const rest = path.slice(base.length);
	if (method === 'GET' && rest === 'support') return json(200, support);
	if (method === 'GET' && rest === 'drafts') return json(200, [...drafts.values()].map(snapshot));
	if (method === 'POST' && rest === 'drafts') {
		if (!body.id || drafts.has(body.id) || (!body.credential?.password && !body.credential?.privateKey)) return json(drafts.has(body.id) ? 409 : 400, { error: drafts.has(body.id) ? 'conflict' : 'bad_request' });
		const view: BootstrapView = { id: body.id, status: 'awaiting-fingerprint', host: body.host, hostKey: { type: 'ssh-ed25519', fingerprint: MOCK_FINGERPRINT }, progress: [], mainApproved: false, lockdownApproved: false, credentialsHeld: true };
		const d: Draft = { view, ticks: 0 };
		(d as Draft & { profile?: BootstrapProfile }).profile = body.profile;
		drafts.set(body.id, d);
		return json(201, snapshot(d)); // the credential is dropped here
	}
	const m = rest.match(/^drafts\/([^/]+)(?:\/([a-z-]+))?$/);
	if (!m) return undefined;
	const d = drafts.get(decodeURIComponent(m[1]));
	if (!d) return json(404, { error: 'not_found' });
	const v = d.view;
	switch (`${method} ${m[2] ?? ''}`) {
		case 'GET ':
			// A poll advances a running mock by one step.
			if (v.status === 'running' && v.plan) {
				const mainCount = v.plan.steps.filter((s) => !s.gate).length;
				d.ticks++;
				if (v.lockdownApproved) { v.status = 'completed'; v.credentialsHeld = false; }
				else if (d.ticks >= mainCount) { v.status = v.plan.lockdownDigest ? 'main-complete' : 'completed'; if (v.status === 'completed') v.credentialsHeld = false; }
			}
			return json(200, snapshot(d));
		case 'GET plan':
			return v.plan ? json(200, v.plan) : json(409, { error: 'conflict' });
		case 'POST fingerprint':
			if (v.status !== 'awaiting-fingerprint') return json(409, { error: 'conflict' });
			if (body.fingerprint !== MOCK_FINGERPRINT) { v.status = 'cancelled'; v.credentialsHeld = false; return json(409, { error: 'conflict' }); }
			v.plan = buildPlan(v.id, v.host.address, v.host.user, (d as Draft & { profile?: BootstrapProfile }).profile as BootstrapProfile);
			v.status = v.plan.blocked ? 'blocked' : 'planned';
			return json(200, snapshot(d));
		case 'POST approve':
			if (!v.plan || v.status !== 'planned') return json(409, { error: 'conflict' });
			if (body.digest !== v.plan.digest) return json(409, { error: 'approval_mismatch' });
			v.mainApproved = true; v.status = 'approved';
			return json(200, snapshot(d));
		case 'POST approve-lockdown':
			if (!v.plan?.lockdownDigest || v.status !== 'main-complete') return json(409, { error: 'conflict' });
			if (body.digest !== v.plan.lockdownDigest) return json(409, { error: 'approval_mismatch' });
			v.lockdownApproved = true;
			return json(200, snapshot(d));
		case 'POST run':
			if (!v.mainApproved || !['approved', 'paused', 'main-complete'].includes(v.status)) return json(409, { error: 'conflict' });
			if (v.status === 'main-complete' && !v.lockdownApproved) return json(409, { error: 'conflict' });
			v.status = 'running'; d.ticks = 0;
			return json(202, snapshot(d));
		case 'POST credentials':
			v.credentialsHeld = true;
			return json(200, snapshot(d));
		case 'POST cancel':
			v.status = 'cancelled'; v.credentialsHeld = false;
			return json(200, snapshot(d));
	}
	return undefined;
}

export function resetBootstrapMock() { drafts.clear(); }
export const MOCK_HOST_FINGERPRINT = MOCK_FINGERPRINT;
