// Pure rules of the vulnerability response pages (X13, ADR 0015). The manager
// decides everything that matters (policy, validation, plan digests); these
// helpers only filter, explain and keep the forms honest before a request.

import type {
	ApiError,
	VulnFeedState,
	VulnFinding,
	VulnMode,
	VulnPolicy,
	VulnPriority,
	VulnProposal,
	VulnProvider,
	VulnProviderInput,
	VulnProviderKind,
	VulnSeverity,
	VulnStatus
} from './api';
import type { MessageKey } from './i18n';

// ---------------------------------------------------------------------------
// Findings

export type FindingFilter = {
	/** 'open' hides not_affected and fixed findings. */
	status: 'open' | 'all' | VulnStatus;
	priority: 'all' | VulnPriority;
	query: string;
};

export const defaultFilter = (): FindingFilter => ({ status: 'open', priority: 'all', query: '' });

const isOpen = (s: VulnStatus) => s === 'affected' || s === 'under_investigation';

export function filterFindings(list: readonly VulnFinding[], f: FindingFilter): VulnFinding[] {
	const q = f.query.trim().toLowerCase();
	return list.filter((x) => {
		if (f.status === 'open' ? !isOpen(x.status) : f.status !== 'all' && x.status !== f.status) return false;
		if (f.priority !== 'all' && x.priority !== f.priority) return false;
		if (q === '') return true;
		const hay = [x.advisoryId, ...(x.aliases ?? []), x.component.name, x.image, ...x.services].join(' ').toLowerCase();
		return hay.includes(q);
	});
}

/** Highest priority first, then score, then ID (the manager already sorts; this keeps the view stable after a status change). */
export function findingOrder(list: readonly VulnFinding[]): VulnFinding[] {
	return [...list].sort((a, b) => a.priority.localeCompare(b.priority) || b.score - a.score || a.id.localeCompare(b.id));
}

export function countByPriority(list: readonly VulnFinding[]): Record<VulnPriority, number> {
	const out: Record<VulnPriority, number> = { P1: 0, P2: 0, P3: 0, P4: 0 };
	for (const f of list) if (isOpen(f.status)) out[f.priority]++;
	return out;
}

export type Tone = 'bad' | 'warn' | 'neutral' | 'ok' | 'unknown';

export const priorityTone = (p: VulnPriority): Tone => ({ P1: 'bad', P2: 'bad', P3: 'warn', P4: 'neutral' } as Record<VulnPriority, Tone>)[p];
export const severityTone = (s: VulnSeverity): Tone =>
	({ critical: 'bad', high: 'bad', medium: 'warn', low: 'neutral', unknown: 'unknown' } as Record<VulnSeverity, Tone>)[s];

/** Findings of an image whose components are only known by reference or not at all are not fully covered. */
export const coverageIsPartial = (f: VulnFinding) => f.coverage !== 'sbom';

export function feedHealth(f: VulnFeedState): 'ok' | 'error' | 'stale' | 'never' {
	if (f.lastError) return 'error';
	if (!f.lastSuccess) return 'never';
	return f.stale ? 'stale' : 'ok';
}

/** Statuses an administrator may set by hand; not_affected needs a justification. */
export const manualStatuses = ['under_investigation', 'not_affected', 'affected'] as const;

export function statusInputError(status: VulnStatus, justification: string): MessageKey | undefined {
	if (status === 'not_affected' && justification.trim() === '') return 'vulnJustificationNeeded';
	if (new TextEncoder().encode(justification).length > 1024) return 'vulnJustificationLong';
	return undefined;
}

// ---------------------------------------------------------------------------
// Proposals

export const proposalsFor = (findingId: string, all: readonly VulnProposal[]) =>
	all.filter((p) => p.findingId === findingId).sort((a, b) => b.createdAt.localeCompare(a.createdAt));

/** The proposal is still open for a decision. */
export const proposalOpen = (p: VulnProposal) => p.status === 'proposed' || p.status === 'awaiting_approval';

/**
 * Why the approve button is disabled, or undefined when approval may be
 * requested. The manager repeats every check and recomputes the plan.
 */
export function approvalBlock(p: VulnProposal, policy: VulnPolicy | undefined): MessageKey | undefined {
	if (!proposalOpen(p)) return 'vulnBlockClosed';
	if (!p.validation.ok) return 'vulnBlockInvalid';
	if (policy?.killSwitch) return 'vulnBlockKillSwitch';
	if (policy?.mode === 'notify_only') return 'vulnBlockNotifyOnly';
	return undefined;
}

/** The approved plan changes nothing but records the decision. */
export const planIsManual = (p: VulnProposal) => p.plan.manual;

export const modeKey = (m: VulnMode): MessageKey =>
	({ notify_only: 'vulnModeNotifyOnly', approval_required: 'vulnModeApproval', auto_for_policy: 'vulnModeAuto' } as Record<VulnMode, MessageKey>)[m];

/** Error codes of the vulnerability endpoints, explained for this feature (the generic texts of other pages do not fit). */
export function vulnErrorKey(e: ApiError): MessageKey | undefined {
	switch (e.code) {
		case 'policy_conflict':
			return 'vulnErrPolicy';
		case 'settings_incomplete':
			return 'vulnErrConsent';
		case 'busy':
			return 'vulnErrBudget';
		case 'approval_mismatch':
			return 'vulnErrDigest';
	}
	if (e.kind === 'conflict') return 'vulnErrConflict';
	return undefined;
}

// ---------------------------------------------------------------------------
// Policy

export const autoSeverities = ['low', 'medium', 'high', 'critical'] as const;
export const autoKinds = ['patched_rebuild', 'mitigation'] as const;

export interface PolicyForm {
	mode: VulnMode;
	provider: string;
	model: string;
	killSwitch: boolean;
	mitigationDays: string;
	retainPrompts: boolean;
	severities: string[];
	/** One service per line; wildcards are refused. */
	services: string;
	kinds: string[];
	maxPerDay: string;
	/** Must be ticked to save auto_for_policy. */
	autoAck: boolean;
}

export type PolicyFormError = 'services' | 'severities' | 'kinds' | 'maxPerDay' | 'mitigationDays' | 'providerModel' | 'autoAck';

export function policyToForm(p: VulnPolicy): PolicyForm {
	return {
		mode: p.mode,
		provider: p.provider ?? '',
		model: p.model ?? '',
		killSwitch: p.killSwitch ?? false,
		mitigationDays: p.mitigationDays ? String(p.mitigationDays) : '',
		retainPrompts: p.retainPrompts ?? false,
		severities: p.auto?.severities ?? [],
		services: (p.auto?.services ?? []).join('\n'),
		kinds: p.auto?.kinds ?? [],
		maxPerDay: p.auto?.maxPerDay ? String(p.auto.maxPerDay) : '',
		// Already opted in: the acknowledgement is not asked again for an unchanged mode.
		autoAck: p.mode === 'auto_for_policy'
	};
}

const lines = (s: string) => s.split('\n').map((x) => x.trim()).filter((x) => x !== '');

export function validatePolicyForm(f: PolicyForm): PolicyFormError[] {
	const out: PolicyFormError[] = [];
	if ((f.provider === '') !== (f.model.trim() === '')) out.push('providerModel');
	if (f.mitigationDays.trim() !== '') {
		const n = Number(f.mitigationDays);
		if (!Number.isInteger(n) || n < 1 || n > 90) out.push('mitigationDays');
	}
	if (f.mode === 'auto_for_policy') {
		const svc = lines(f.services);
		if (svc.length === 0 || svc.some((s) => s === '*' || s.includes('*') || s.length > 128)) out.push('services');
		if (f.severities.length === 0) out.push('severities');
		if (f.kinds.length === 0) out.push('kinds');
		const n = Number(f.maxPerDay);
		if (!Number.isInteger(n) || n < 1 || n > 20) out.push('maxPerDay');
		if (!f.autoAck) out.push('autoAck');
	}
	return out;
}

export function policyFromForm(f: PolicyForm): VulnPolicy {
	const p: VulnPolicy = { mode: f.mode, killSwitch: f.killSwitch, retainPrompts: f.retainPrompts };
	if (f.provider !== '') {
		p.provider = f.provider;
		p.model = f.model.trim();
	}
	if (f.mitigationDays.trim() !== '') p.mitigationDays = Number(f.mitigationDays);
	if (f.mode === 'auto_for_policy') {
		p.auto = { severities: f.severities, services: lines(f.services), kinds: f.kinds, maxPerDay: Number(f.maxPerDay) };
	}
	return p;
}

/** Selecting auto_for_policy for the first time needs the explicit acknowledgement; leaving it clears it. */
export const needsAutoAck = (stored: VulnPolicy, f: PolicyForm) => f.mode === 'auto_for_policy' && stored.mode !== 'auto_for_policy';

// ---------------------------------------------------------------------------
// Providers

export const providerKindLabel: Record<VulnProviderKind, MessageKey> = {
	claude_api: 'vulnKindClaude',
	openai_api: 'vulnKindOpenAI',
	openai_compatible: 'vulnKindCompatible',
	claude_code_cli: 'vulnKindClaudeCli',
	codex_cli: 'vulnKindCodexCli'
};

export const isCliKind = (k: VulnProviderKind) => k === 'claude_code_cli' || k === 'codex_cli';
export const needsApiKey = (k: VulnProviderKind) => k === 'claude_api' || k === 'openai_api';

export interface ProviderForm {
	id: string;
	name: string;
	kind: VulnProviderKind;
	baseUrl: string;
	/** Comma separated allow-list; empty allows any syntactically valid model. */
	models: string;
	defaultModel: string;
	binary: string;
	home: string;
	experimental: boolean;
	apiKey: string;
	requestsPerDay: string;
	tokensPerDay: string;
}

export type ProviderFormError = 'id' | 'name' | 'baseUrl' | 'apiKey' | 'binary' | 'experimental' | 'defaultModel' | 'limits';

export const emptyProviderForm = (): ProviderForm => ({
	id: '',
	name: '',
	kind: 'claude_api',
	baseUrl: '',
	models: '',
	defaultModel: '',
	binary: '',
	home: '',
	experimental: false,
	apiKey: '',
	requestsPerDay: '',
	tokensPerDay: ''
});

const providerIdRe = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/;
const modelList = (s: string) => s.split(',').map((x) => x.trim()).filter((x) => x !== '');

export function validateProviderForm(f: ProviderForm): ProviderFormError[] {
	const out: ProviderFormError[] = [];
	if (!providerIdRe.test(f.id)) out.push('id');
	if (f.name.trim() === '' || f.name.length > 100) out.push('name');
	if (f.kind === 'openai_compatible' && f.baseUrl.trim() === '') out.push('baseUrl');
	if (f.baseUrl.trim() !== '' && !/^https?:\/\/[^/\s]+/.test(f.baseUrl.trim())) out.push('baseUrl');
	if (needsApiKey(f.kind) && f.apiKey.trim() === '') out.push('apiKey');
	if (isCliKind(f.kind)) {
		if (!f.binary.startsWith('/') || f.binary.includes('..')) out.push('binary');
		if (!f.experimental) out.push('experimental');
	}
	const models = modelList(f.models);
	if (f.defaultModel.trim() !== '' && models.length > 0 && !models.includes(f.defaultModel.trim())) out.push('defaultModel');
	for (const n of [f.requestsPerDay, f.tokensPerDay]) {
		if (n.trim() !== '' && (!Number.isInteger(Number(n)) || Number(n) < 1)) {
			out.push('limits');
			break;
		}
	}
	return out;
}

/** The request body. Fields that do not apply to the kind are left out; the key is sent once. */
export function buildProviderInput(f: ProviderForm): VulnProviderInput {
	const cli = isCliKind(f.kind);
	const input: VulnProviderInput = { id: f.id.trim(), name: f.name.trim(), kind: f.kind };
	const models = modelList(f.models);
	if (models.length > 0) input.models = models;
	if (f.defaultModel.trim() !== '') input.defaultModel = f.defaultModel.trim();
	if (cli) {
		input.binary = f.binary;
		if (f.home.trim() !== '') input.home = f.home.trim();
		input.experimental = true;
	} else {
		if (f.baseUrl.trim() !== '') input.baseUrl = f.baseUrl.trim();
		if (needsApiKey(f.kind) && f.apiKey !== '') input.apiKey = f.apiKey;
	}
	const limits: { requestsPerDay?: number; tokensPerDay?: number } = {};
	if (f.requestsPerDay.trim() !== '') limits.requestsPerDay = Number(f.requestsPerDay);
	if (f.tokensPerDay.trim() !== '') limits.tokensPerDay = Number(f.tokensPerDay);
	if (Object.keys(limits).length > 0) input.limits = limits;
	return input;
}

/** A provider can be used for analysis only with consent; local providers still need it. */
export const providerUsable = (p: VulnProvider) => p.consent?.by !== undefined && p.consent.by !== '';

/** Models offered in the policy for a provider (its allow-list, else its default model). */
export function modelChoices(p: VulnProvider | undefined): string[] {
	if (!p) return [];
	if (p.models && p.models.length > 0) return p.models;
	return p.defaultModel ? [p.defaultModel] : [];
}

/**
 * Consent is granted by echoing the provider's current egress statement. The
 * button needs the acknowledgement and a statement to send.
 */
export function consentRequest(p: VulnProvider, acknowledged: boolean): { statement: string } | undefined {
	if (!acknowledged || p.egressStatement === '') return undefined;
	return { statement: p.egressStatement };
}

// ---------------------------------------------------------------------------
// Labels

export const statusLabel: Record<VulnStatus, MessageKey> = {
	affected: 'vulnStatusAffected',
	under_investigation: 'vulnStatusUnderInvestigation',
	not_affected: 'vulnStatusNotAffected',
	fixed: 'vulnStatusFixed'
};
export const severityLabel: Record<VulnSeverity, MessageKey> = {
	unknown: 'vulnSevUnknown',
	low: 'vulnSevLow',
	medium: 'vulnSevMedium',
	high: 'vulnSevHigh',
	critical: 'vulnSevCritical'
};
export const coverageLabel: Record<string, MessageKey> = { sbom: 'vulnCovSbom', reference: 'vulnCovReference', unknown: 'vulnCovUnknown' };
export const serviceKindLabel: Record<string, MessageKey> = {
	web: 'vulnSvcWeb',
	generic: 'vulnSvcGeneric',
	database: 'vulnSvcDatabase',
	galera: 'vulnSvcGalera',
	proxy: 'vulnSvcProxy',
	registry: 'vulnSvcRegistry'
};
export const proposalKindLabel: Record<string, MessageKey> = { patched_rebuild: 'vulnKindPatchedRebuild', mitigation: 'vulnKindMitigation', no_op: 'vulnKindNoOp' };
export const proposalStatusLabel: Record<string, MessageKey> = {
	proposed: 'vulnPStatusProposed',
	awaiting_approval: 'vulnPStatusAwaiting',
	invalid: 'vulnPStatusInvalid',
	approved: 'vulnPStatusApproved',
	declined: 'vulnPStatusDeclined',
	failed: 'vulnPStatusFailed'
};
export const levelLabel: Record<string, MessageKey> = { none: 'vulnLevelNone', low: 'vulnLevelLow', medium: 'vulnLevelMedium', high: 'vulnLevelHigh' };
export const feedErrorLabel: Record<string, MessageKey> = {
	rate_limited: 'vulnFeedErrRateLimited',
	too_large: 'vulnFeedErrTooLarge',
	malformed: 'vulnFeedErrMalformed',
	http_status: 'vulnFeedErrHttpStatus',
	timeout: 'vulnFeedErrTimeout',
	transport: 'vulnFeedErrTransport'
};

export const toneText: Record<Tone, string> = {
	bad: 'text-red-700 dark:text-red-400',
	warn: 'text-amber-700 dark:text-amber-400',
	neutral: 'text-sky-700 dark:text-sky-400',
	ok: 'text-emerald-700 dark:text-emerald-400',
	unknown: 'text-muted-foreground'
};

/** Newest committed audit sequence first; never mutate the loaded response. */
export function auditRecords(records: import('./api').VulnAuditRecord[], query: string) {
 const q = query.trim().toLowerCase();
 return records.filter(r => [r.actor, r.event, r.result, r.detail, r.findingId, r.proposalId, r.provider, r.model, r.planDigest].some(v => v?.toLowerCase().includes(q)))
  .sort((a, b) => b.seq - a.seq || b.id.localeCompare(a.id));
}
