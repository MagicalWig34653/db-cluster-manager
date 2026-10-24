import { describe, expect, it } from 'vitest';
import { ApiError, createClient, isVulnFinding, isVulnProposal, type VulnFinding, type VulnPolicy, type VulnProposal, type VulnProvider } from './api';
import {
	approvalBlock,
	buildProviderInput,
	consentRequest,
	countByPriority,
	defaultFilter,
	emptyProviderForm,
	feedHealth,
	filterFindings,
	findingOrder,
	needsAutoAck,
	policyFromForm,
	policyToForm,
	statusInputError,
	validatePolicyForm,
	validateProviderForm,
	vulnErrorKey
} from './vuln';
import { vulnMock } from './mock-vuln';

const finding = (o: Partial<VulnFinding> = {}): VulnFinding => ({
	id: 'f1', advisoryId: 'CVE-2026-1', entryId: 'e', image: 'reg/team/web:1', services: ['sw/web'], serviceKind: 'web', coverage: 'reference',
	component: { ecosystem: 'docker', name: 'team/web', version: '1' }, severity: 'high', score: 70, priority: 'P2', status: 'affected',
	firstSeen: '2026-10-01T00:00:00Z', lastSeen: '2026-10-02T00:00:00Z', sources: ['osv'], ...o
});
const proposal = (o: Partial<VulnProposal> = {}): VulnProposal => ({
	id: 'p1', findingId: 'f1', advisoryId: 'CVE-2026-1', entryId: 'e', kind: 'patched_rebuild', origin: 'deterministic',
	component: { ecosystem: 'docker', name: 'team/web', version: '1' }, rating: { benefit: 'high', risk: 'low', basis: 'b' },
	plan: { digest: 'sha256:aa', manual: false, steps: [], warnings: [] }, validation: { ok: true },
	decision: { mode: 'approval_required', action: 'await_approval' }, status: 'awaiting_approval', createdBy: 'a', createdAt: '2026-10-01T00:00:00Z', ...o
});

describe('findings', () => {
	const list = [
		finding({ id: 'a', priority: 'P3', score: 40 }),
		finding({ id: 'b', priority: 'P1', score: 90, advisoryId: 'CVE-2026-9' }),
		finding({ id: 'c', priority: 'P1', score: 95, status: 'fixed' }),
		finding({ id: 'd', priority: 'P2', status: 'not_affected' })
	];
	it('hides closed findings by default and filters by priority and text', () => {
		expect(filterFindings(list, defaultFilter()).map((f) => f.id)).toEqual(['a', 'b']);
		expect(filterFindings(list, { ...defaultFilter(), status: 'all' })).toHaveLength(4);
		expect(filterFindings(list, { ...defaultFilter(), priority: 'P1' }).map((f) => f.id)).toEqual(['b']);
		expect(filterFindings(list, { ...defaultFilter(), query: ' cve-2026-9 ' }).map((f) => f.id)).toEqual(['b']);
	});
	it('orders by priority then score and counts open findings', () => {
		expect(findingOrder(list).map((f) => f.id)).toEqual(['c', 'b', 'd', 'a']);
		expect(countByPriority(list)).toEqual({ P1: 1, P2: 0, P3: 1, P4: 0 });
	});
	it('requires a justification for not_affected', () => {
		expect(statusInputError('not_affected', ' ')).toBe('vulnJustificationNeeded');
		expect(statusInputError('not_affected', 'unreachable')).toBeUndefined();
		expect(statusInputError('under_investigation', '')).toBeUndefined();
	});
	it('classifies feed health', () => {
		expect(feedHealth({ name: 'x', stale: false, lastSuccess: '2026-10-01T00:00:00Z' })).toBe('ok');
		expect(feedHealth({ name: 'x', stale: true, lastSuccess: '2026-10-01T00:00:00Z' })).toBe('stale');
		expect(feedHealth({ name: 'x', stale: true })).toBe('never');
		expect(feedHealth({ name: 'x', stale: true, lastError: 'timeout', lastSuccess: '2026-10-01T00:00:00Z' })).toBe('error');
	});
});

describe('approval', () => {
	const mode = (m: VulnPolicy['mode'], extra: Partial<VulnPolicy> = {}): VulnPolicy => ({ mode: m, ...extra });
	it('explains why approval is blocked', () => {
		expect(approvalBlock(proposal(), mode('approval_required'))).toBeUndefined();
		expect(approvalBlock(proposal(), mode('notify_only'))).toBe('vulnBlockNotifyOnly');
		expect(approvalBlock(proposal(), mode('approval_required', { killSwitch: true }))).toBe('vulnBlockKillSwitch');
		expect(approvalBlock(proposal({ validation: { ok: false, errors: ['x'] }, status: 'invalid' }), mode('approval_required'))).toBe('vulnBlockClosed');
		expect(approvalBlock(proposal({ status: 'proposed', validation: { ok: false } }), mode('approval_required'))).toBe('vulnBlockInvalid');
		expect(approvalBlock(proposal({ status: 'approved' }), mode('approval_required'))).toBe('vulnBlockClosed');
	});
	it('maps coded errors to feature texts', () => {
		expect(vulnErrorKey(new ApiError('conflict', 409, undefined, 'approval_mismatch'))).toBe('vulnErrDigest');
		expect(vulnErrorKey(new ApiError('conflict', 409, undefined, 'policy_conflict'))).toBe('vulnErrPolicy');
		expect(vulnErrorKey(new ApiError('conflict', 409, undefined, 'settings_incomplete'))).toBe('vulnErrConsent');
		expect(vulnErrorKey(new ApiError('conflict', 409, undefined, 'busy'))).toBe('vulnErrBudget');
		expect(vulnErrorKey(new ApiError('conflict', 409))).toBe('vulnErrConflict');
		expect(vulnErrorKey(new ApiError('network', 0))).toBeUndefined();
	});
});

describe('policy form', () => {
	const stored: VulnPolicy = { mode: 'notify_only', mitigationDays: 30 };
	it('requires allow-list, limit and acknowledgement for auto_for_policy', () => {
		const f = { ...policyToForm(stored), mode: 'auto_for_policy' as const };
		expect(validatePolicyForm(f).sort()).toEqual(['autoAck', 'kinds', 'maxPerDay', 'services', 'severities']);
		expect(needsAutoAck(stored, f)).toBe(true);
		const ok = { ...f, severities: ['critical'], kinds: ['patched_rebuild'], services: 'sw/shop-web', maxPerDay: '3', autoAck: true };
		expect(validatePolicyForm(ok)).toEqual([]);
		expect(policyFromForm(ok).auto).toEqual({ severities: ['critical'], kinds: ['patched_rebuild'], services: ['sw/shop-web'], maxPerDay: 3 });
	});
	it('refuses wildcards, big limits and half provider choices', () => {
		const base = { ...policyToForm(stored), mode: 'auto_for_policy' as const, severities: ['high'], kinds: ['mitigation'], autoAck: true };
		expect(validatePolicyForm({ ...base, services: 'sw/*', maxPerDay: '3' })).toContain('services');
		expect(validatePolicyForm({ ...base, services: 'a', maxPerDay: '21' })).toContain('maxPerDay');
		expect(validatePolicyForm({ ...policyToForm(stored), provider: 'p', model: '' })).toContain('providerModel');
		expect(validatePolicyForm({ ...policyToForm(stored), mitigationDays: '91' })).toContain('mitigationDays');
	});
	it('does not send auto settings for other modes and keeps an existing opt-in without a new acknowledgement', () => {
		expect(policyFromForm(policyToForm({ mode: 'approval_required' })).auto).toBeUndefined();
		const auto: VulnPolicy = { mode: 'auto_for_policy', auto: { severities: ['high'], services: ['a'], kinds: ['mitigation'], maxPerDay: 2 }, autoOptInBy: 'x' };
		expect(needsAutoAck(auto, policyToForm(auto))).toBe(false);
		expect(validatePolicyForm(policyToForm(auto))).toEqual([]);
	});
});

describe('provider form', () => {
	it('needs an API key for hosted kinds and acknowledgement for CLI kinds', () => {
		const f = { ...emptyProviderForm(), id: 'p', name: 'P' };
		expect(validateProviderForm(f)).toEqual(['apiKey']);
		expect(validateProviderForm({ ...f, apiKey: 'k' })).toEqual([]);
		const cli = { ...f, kind: 'claude_code_cli' as const, binary: 'claude' };
		expect(validateProviderForm(cli).sort()).toEqual(['binary', 'experimental']);
		expect(validateProviderForm({ ...f, kind: 'openai_compatible' })).toEqual(['baseUrl']);
	});
	it('builds a request that carries only applicable fields', () => {
		const f = { ...emptyProviderForm(), id: 'p', name: 'P', apiKey: 'secret', models: 'm1, m2', defaultModel: 'm1', requestsPerDay: '10', baseUrl: 'https://x.example' };
		expect(buildProviderInput(f)).toEqual({ id: 'p', name: 'P', kind: 'claude_api', models: ['m1', 'm2'], defaultModel: 'm1', baseUrl: 'https://x.example', apiKey: 'secret', limits: { requestsPerDay: 10 } });
		const cli = buildProviderInput({ ...f, kind: 'codex_cli', binary: '/usr/local/bin/codex', experimental: true });
		expect(cli.apiKey).toBeUndefined();
		expect(cli.baseUrl).toBeUndefined();
		expect(cli.experimental).toBe(true);
	});
	it('sends consent only with the acknowledgement and the exact statement', () => {
		const p = { id: 'p', egressStatement: 'S' } as VulnProvider;
		expect(consentRequest(p, false)).toBeUndefined();
		expect(consentRequest(p, true)).toEqual({ statement: 'S' });
	});
});

describe('contract guards and mock server', () => {
	const client = createClient((input, init) => {
		const url = new URL(String(input), 'http://localhost');
		const body = init?.body ? JSON.parse(String(init.body)) : {};
		return Promise.resolve(vulnMock((init?.method ?? 'GET').toUpperCase(), url.pathname, body) ?? new Response('{}', { status: 404 }));
	});
	it('accepts every mock finding and proposal and null lists from the Go API', async () => {
		for (const f of await client.vulnFindings()) expect(isVulnFinding(f)).toBe(true);
		const p = await client.proposeVulnRemediation('f-php-upload', false);
		expect(isVulnProposal(p)).toBe(true);
		expect(isVulnFinding({ ...finding(), services: null, sources: null })).toBe(true);
		expect(isVulnFinding({ ...finding(), priority: 'P9' })).toBe(false);
	});
	it('refuses approval under notify_only and on a wrong digest, then approves exactly the shown plan', async () => {
		const p = await client.proposeVulnRemediation('f-php-upload', false);
		await expect(client.approveVulnProposal(p.id, p.plan.digest)).rejects.toMatchObject({ code: 'policy_conflict' });
		const { version } = await client.vulnPolicy();
		await client.saveVulnPolicy({ mode: 'approval_required' }, version);
		await expect(client.approveVulnProposal(p.id, 'sha256:wrong')).rejects.toMatchObject({ code: 'approval_mismatch' });
		expect((await client.approveVulnProposal(p.id, p.plan.digest)).status).toBe('approved');
		await expect(client.approveVulnProposal(p.id, p.plan.digest)).rejects.toMatchObject({ kind: 'conflict' });
	});
	it('rejects auto_for_policy without an allow-list and a stale policy version', async () => {
		const { version } = await client.vulnPolicy();
		await expect(client.saveVulnPolicy({ mode: 'auto_for_policy' }, version)).rejects.toMatchObject({ kind: 'bad_request' });
		await expect(client.saveVulnPolicy({ mode: 'approval_required' }, version - 1)).rejects.toMatchObject({ kind: 'conflict' });
	});
	it('needs the exact egress statement for consent and never returns the API key', async () => {
		const created = await client.createVulnProvider({ id: 'new', name: 'New', kind: 'claude_api', apiKey: 'sk-secret' });
		expect(JSON.stringify(created)).not.toContain('sk-secret');
		expect(created.credentialSet).toBe(true);
		await expect(client.setVulnProviderConsent('new', true, 'something else')).rejects.toMatchObject({ code: 'approval_mismatch' });
		expect((await client.setVulnProviderConsent('new', true, created.egressStatement)).consent?.by).toBe('mock-admin');
		expect((await client.setVulnProviderConsent('new', false)).consent).toBeUndefined();
	});
});
