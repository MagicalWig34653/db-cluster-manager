// Typed client for the manager API (api/openapi.yaml), with small hand-written
// runtime guards. Responses that do not match the contract are rejected as
// `invalid_response` instead of being shown partially.
//
// CSRF protection relies on the browser: same-origin fetch POSTs carry an
// Origin (and Sec-Fetch-Site) header automatically, which the manager checks.
// Server error bodies carry only a coarse code; it is used for classification
// and never rendered verbatim.

import type { CredentialJSON } from './webauthn';

const BASE = '/api/v1';

// ---------------------------------------------------------------------------
// Types (mirror components/schemas in api/openapi.yaml)

export interface VersionInfo {
	component: string;
	version: string;
	commit: string;
	goVersion: string;
	platform: string;
}

export const states = ['healthy', 'degraded', 'unavailable', 'unknown'] as const;
export type State = (typeof states)[number];
export const operationStates = ['pending', 'running', 'succeeded', 'failed', 'unknown'] as const;
export type OperationState = (typeof operationStates)[number];
export const severities = ['info', 'warning', 'critical', 'unknown'] as const;
export type Severity = (typeof severities)[number];

export interface Swarm {
	id: string;
	name: string;
	state: State;
	observedAt?: string;
}

export interface ClusterNode {
	id: string;
	host?: string;
	state: State;
	/** Proposed field (cluster/health work): "primary", "donor", "joiner", "arbitrator" ... Unknown when absent. */
	role?: string;
	/** Proposed field: time of the last health observation of this node. */
	observedAt?: string;
}

/** Proposed field of Cluster: client endpoint (e.g. proxy VIP or service name). */
export interface Endpoint {
	name?: string;
	host: string;
	port: number;
}

export interface Cluster {
	id: string;
	swarm: string;
	name: string;
	engine?: string;
	state: State;
	nodes: ClusterNode[];
	observedAt?: string;
	/** Proposed optional fields; absent means "not reported", never "none". */
	endpoints?: Endpoint[];
	lastBackupAt?: string;
	lastVerifiedRestoreAt?: string;
	automationPolicy?: string;
}

export interface Operation {
	id: string;
	kind: string;
	swarm?: string;
	cluster?: string;
	state: OperationState;
	startedAt?: string;
	endedAt?: string;
}

export interface ManagerEvent {
	id: string;
	time: string;
	swarm?: string;
	cluster?: string;
	severity: Severity;
	message: string;
}

export interface SessionInfo {
	accountId: string;
	restricted: boolean;
	pendingPasskeyEnrollment: boolean;
	createdAt: string;
}

export interface Challenge {
	ceremonyId: string;
	options: Record<string, unknown>;
	expiresAt: string;
}

export interface PasskeyInfo {
	id: string;
	accountId: string;
	rpId: string;
	label?: string;
	createdAt: string;
	signCount: number;
	backupEligible: boolean;
	backupState: boolean;
}

export interface SetupResult {
	accountId: string;
	recoveryCodes: string[] | null;
}

// ---------------------------------------------------------------------------
// Errors

/** Coarse classification used by the UI; never contains server text. */
export type ErrorKind =
	| 'unauthenticated'
	| 'forbidden'
	| 'origin_rejected'
	| 'passkey_enrollment_required'
	| 'not_found'
	| 'bad_request'
	| 'conflict'
	| 'rate_limited'
	| 'unavailable'
	| 'server_error'
	| 'invalid_response'
	| 'network';

/**
 * Coarse codes of the (not yet documented) write endpoints that the UI turns
 * into explanations. Anything else is dropped so server text is never kept.
 */
export const knownCodes = [
	'requires_guided_recovery',
	'no_capacity',
	'policy_conflict',
	'settings_incomplete',
	'cluster_exists',
	'not_leader'
] as const;
export type KnownCode = (typeof knownCodes)[number];
const isKnownCode = (v: unknown): v is KnownCode => isStr(v) && (knownCodes as readonly string[]).includes(v);

export class ApiError extends Error {
	constructor(
		readonly kind: ErrorKind,
		readonly status: number,
		/** Seconds from Retry-After on 429, if given. */
		readonly retryAfter?: number,
		/** Allowlisted coarse server code (see knownCodes); never arbitrary server text. */
		readonly code?: KnownCode
	) {
		super(`API error: ${kind} (${status})`);
		this.name = 'ApiError';
	}
}

function classify(status: number, code: string | undefined): ErrorKind {
	if (status === 401) return 'unauthenticated';
	if (status === 403) {
		if (code === 'origin_rejected') return 'origin_rejected';
		if (code === 'passkey_enrollment_required') return 'passkey_enrollment_required';
		return 'forbidden';
	}
	if (status === 404) return 'not_found';
	if (status === 409) return 'conflict';
	if (status === 429) return 'rate_limited';
	if (status === 503) return 'unavailable';
	if (status >= 400 && status < 500) return 'bad_request';
	return 'server_error';
}

export function asApiError(e: unknown): ApiError {
	return e instanceof ApiError ? e : new ApiError('network', 0);
}

// ---------------------------------------------------------------------------
// Guards

type Guard<T> = (v: unknown) => v is T;

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const isStr = (v: unknown): v is string => typeof v === 'string';
const optStr = (v: unknown) => v === undefined || isStr(v);
const isBool = (v: unknown): v is boolean => typeof v === 'boolean';
const isDate = (v: unknown) => isStr(v) && !Number.isNaN(Date.parse(v));
const optDate = (v: unknown) => v === undefined || isDate(v);
const oneOf =
	<T extends string>(values: readonly T[]) =>
	(v: unknown): v is T =>
		isStr(v) && (values as readonly string[]).includes(v);

export const isState = oneOf(states);
export const isOperationState = oneOf(operationStates);
export const isSeverity = oneOf(severities);

export const isVersionInfo: Guard<VersionInfo> = (v): v is VersionInfo =>
	isObj(v) && ['component', 'version', 'commit', 'goVersion', 'platform'].every((k) => isStr(v[k]));

export const isSwarm: Guard<Swarm> = (v): v is Swarm =>
	isObj(v) && isStr(v.id) && isStr(v.name) && isState(v.state) && optDate(v.observedAt);

export const isClusterNode: Guard<ClusterNode> = (v): v is ClusterNode =>
	isObj(v) && isStr(v.id) && optStr(v.host) && isState(v.state) && optStr(v.role) && optDate(v.observedAt);

const isEndpoint: Guard<Endpoint> = (v): v is Endpoint =>
	isObj(v) && optStr(v.name) && isStr(v.host) && typeof v.port === 'number';

export const isCluster: Guard<Cluster> = (v): v is Cluster =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.swarm) &&
	isStr(v.name) &&
	optStr(v.engine) &&
	isState(v.state) &&
	Array.isArray(v.nodes) &&
	v.nodes.every(isClusterNode) &&
	optDate(v.observedAt) &&
	(v.endpoints === undefined || (Array.isArray(v.endpoints) && v.endpoints.every(isEndpoint))) &&
	optDate(v.lastBackupAt) &&
	optDate(v.lastVerifiedRestoreAt) &&
	optStr(v.automationPolicy);

export const isOperation: Guard<Operation> = (v): v is Operation =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.kind) &&
	optStr(v.swarm) &&
	optStr(v.cluster) &&
	isOperationState(v.state) &&
	optDate(v.startedAt) &&
	optDate(v.endedAt);

export const isEvent: Guard<ManagerEvent> = (v): v is ManagerEvent =>
	isObj(v) &&
	isStr(v.id) &&
	isDate(v.time) &&
	optStr(v.swarm) &&
	optStr(v.cluster) &&
	isSeverity(v.severity) &&
	isStr(v.message);

export const isSessionInfo: Guard<SessionInfo> = (v): v is SessionInfo =>
	isObj(v) && isStr(v.accountId) && isBool(v.restricted) && isBool(v.pendingPasskeyEnrollment) && isDate(v.createdAt);

export const isChallenge: Guard<Challenge> = (v): v is Challenge =>
	isObj(v) && isStr(v.ceremonyId) && isObj(v.options) && isDate(v.expiresAt);

export const isPasskeyInfo: Guard<PasskeyInfo> = (v): v is PasskeyInfo =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.accountId) &&
	isStr(v.rpId) &&
	optStr(v.label) &&
	isDate(v.createdAt) &&
	typeof v.signCount === 'number' &&
	isBool(v.backupEligible) &&
	isBool(v.backupState);

const isSetupResult: Guard<SetupResult> = (v): v is SetupResult =>
	isObj(v) &&
	isStr(v.accountId) &&
	(v.recoveryCodes === null || (Array.isArray(v.recoveryCodes) && v.recoveryCodes.every(isStr)));

const listOf =
	<T>(item: Guard<T>): Guard<{ items: T[] }> =>
	(v): v is { items: T[] } =>
		isObj(v) && Array.isArray(v.items) && v.items.every(item);

const isAny = (_v: unknown): _v is unknown => true;

// ---------------------------------------------------------------------------
// Proposed contract (not in api/openapi.yaml yet). Field names align with
// internal/manager (HostRecord, CreateClusterRequest) and internal/galera
// (Resources, PlacementPolicy, Diagnosis); see docs/DESIGN.md.

export const enrollments = ['discovered', 'enrolled', 'revoked'] as const;
export type Enrollment = (typeof enrollments)[number];

/** Mirrors manager.HostRecord (JSON names), plus proposed agentVersion/lastSeen. */
export interface Host {
	swarm: string;
	nodeId: string;
	hostname?: string;
	role?: string;
	availability?: string;
	state?: string;
	engineVersion?: string;
	enrollment: Enrollment;
	spare?: boolean;
	nanoCpus?: number;
	memoryBytes?: number;
	observedAt?: string;
	/** Proposed. */
	agentVersion?: string;
	/** Proposed. */
	lastSeen?: string;
}

/** Mirrors galera.ResourceAmount. */
export interface ResourceAmount {
	milli_cpu?: number;
	memory_bytes?: number;
}

/** Mirrors manager.CreateClusterRequest. */
export interface CreateClusterRequest {
	swarm: string;
	name: string;
	size: number;
	image: string;
	network: string;
	rootPasswordSecret: string;
	gcacheSize?: string;
	resources: { limits: ResourceAmount; reservations: ResourceAmount };
	placement: { allowlist?: string[]; excluded?: string[]; preferred?: string[]; reserve: ResourceAmount };
	autoReplace?: boolean;
}

export interface Risk {
	kind: string;
	slot?: string;
	detail: string;
}

export interface NodePosition {
	slot: string;
	source: string;
	seqno?: number;
	uuid?: string;
}

/** Mirrors galera.Diagnosis (advice only; requires_approval is always true). */
export interface Diagnosis {
	candidate?: string;
	positions: NodePosition[];
	reasons: string[];
	risks?: Risk[];
	requires_approval: boolean;
}

export interface BackupSummary {
	cluster: string;
	policy?: string;
	lastBackupAt?: string;
	lastVerifiedRestoreAt?: string;
}

export type PanelKind = 'line' | 'stat';
export interface Panel {
	id: string;
	kind: PanelKind;
	title: string;
	query: string;
	unit?: string;
}
export interface DashboardInput {
	name: string;
	shared: boolean;
	panels: Panel[];
}
export interface Dashboard extends DashboardInput {
	id: string;
	owner?: string;
	updatedAt?: string;
}

export interface MetricsQuery {
	query: string;
	from: string;
	to: string;
	/** Step in seconds. */
	step: number;
}
/** points are [unixSeconds, value|null]; null (or a missing step) is a gap. */
export interface MetricSeries {
	labels?: Record<string, string>;
	points: [number, number | null][];
}
export interface MetricsResult {
	series: MetricSeries[];
}

export interface AppNotification {
	id: string;
	time: string;
	severity: Severity;
	message: string;
	cluster?: string;
	read: boolean;
	acknowledged: boolean;
}

export interface NotificationSettings {
	webhook: { enabled: boolean; url?: string; secretSet: boolean };
	email: { enabled: boolean; recipients: string[] };
}
/** The webhook secret is write-only: sent only when changed, never returned. */
export interface NotificationSettingsInput {
	webhook: { enabled: boolean; url: string; secret?: string };
	email: { enabled: boolean; recipients: string[] };
}

const isNum = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v);
const optNum = (v: unknown) => v === undefined || isNum(v);
const strArray = (v: unknown): v is string[] => Array.isArray(v) && v.every(isStr);

export const isHost: Guard<Host> = (v): v is Host =>
	isObj(v) &&
	isStr(v.swarm) &&
	isStr(v.nodeId) &&
	optStr(v.hostname) &&
	optStr(v.role) &&
	optStr(v.availability) &&
	optStr(v.state) &&
	optStr(v.engineVersion) &&
	oneOf(enrollments)(v.enrollment) &&
	(v.spare === undefined || isBool(v.spare)) &&
	optNum(v.nanoCpus) &&
	optNum(v.memoryBytes) &&
	optDate(v.observedAt) &&
	optStr(v.agentVersion) &&
	optDate(v.lastSeen);

const isOperationRef = (v: unknown): v is { id: string; kind?: string } => isObj(v) && isStr(v.id) && optStr(v.kind);

export const isDiagnosis: Guard<Diagnosis> = (v): v is Diagnosis =>
	isObj(v) &&
	optStr(v.candidate) &&
	Array.isArray(v.positions) &&
	v.positions.every((p) => isObj(p) && isStr(p.slot) && isStr(p.source) && optNum(p.seqno) && optStr(p.uuid)) &&
	strArray(v.reasons) &&
	(v.risks === undefined ||
		(Array.isArray(v.risks) && v.risks.every((r) => isObj(r) && isStr(r.kind) && optStr(r.slot) && isStr(r.detail)))) &&
	isBool(v.requires_approval);

export const isBackupSummary: Guard<BackupSummary> = (v): v is BackupSummary =>
	isObj(v) && isStr(v.cluster) && optStr(v.policy) && optDate(v.lastBackupAt) && optDate(v.lastVerifiedRestoreAt);

const isPanel = (v: unknown): v is Panel =>
	isObj(v) && isStr(v.id) && oneOf(['line', 'stat'] as const)(v.kind) && isStr(v.title) && isStr(v.query) && optStr(v.unit);

export const isDashboard: Guard<Dashboard> = (v): v is Dashboard =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.name) &&
	isBool(v.shared) &&
	optStr(v.owner) &&
	optDate(v.updatedAt) &&
	Array.isArray(v.panels) &&
	v.panels.every(isPanel);

export const isMetricsResult: Guard<MetricsResult> = (v): v is MetricsResult =>
	isObj(v) &&
	Array.isArray(v.series) &&
	v.series.every(
		(s) =>
			isObj(s) &&
			Array.isArray(s.points) &&
			s.points.every((p) => Array.isArray(p) && p.length === 2 && isNum(p[0]) && (p[1] === null || isNum(p[1])))
	);

export const isNotification: Guard<AppNotification> = (v): v is AppNotification =>
	isObj(v) &&
	isStr(v.id) &&
	isDate(v.time) &&
	isSeverity(v.severity) &&
	isStr(v.message) &&
	optStr(v.cluster) &&
	isBool(v.read) &&
	isBool(v.acknowledged);

export const isNotificationSettings: Guard<NotificationSettings> = (v): v is NotificationSettings =>
	isObj(v) &&
	isObj(v.webhook) &&
	isBool(v.webhook.enabled) &&
	optStr(v.webhook.url) &&
	isBool(v.webhook.secretSet) &&
	isObj(v.email) &&
	isBool(v.email.enabled) &&
	strArray(v.email.recipients);

// ---------------------------------------------------------------------------
// Client

export type FetchFn = typeof fetch;

export function createClient(fetchFn: FetchFn) {
	async function call<T>(method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, guard: Guard<T>, body?: unknown): Promise<T> {
		const init: RequestInit = {
			method,
			credentials: 'same-origin',
			headers: { Accept: 'application/json', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
			body: body !== undefined ? JSON.stringify(body) : undefined
		};
		let res: Response;
		try {
			res = await fetchFn(BASE + path, init);
		} catch {
			throw new ApiError('network', 0);
		}
		if (!res.ok) {
			let code: string | undefined;
			try {
				const b: unknown = await res.json();
				if (isObj(b) && isStr(b.error)) code = b.error;
			} catch {
				// non-JSON error body: classify by status only
			}
			const ra = Number(res.headers.get('Retry-After'));
			throw new ApiError(
				classify(res.status, code),
				res.status,
				Number.isFinite(ra) && ra > 0 ? ra : undefined,
				isKnownCode(code) ? code : undefined
			);
		}
		if (res.status === 204) {
			if (guard(undefined)) return undefined as T;
			throw new ApiError('invalid_response', res.status);
		}
		let data: unknown;
		try {
			data = await res.json();
		} catch {
			throw new ApiError('invalid_response', res.status);
		}
		if (!guard(data)) throw new ApiError('invalid_response', res.status);
		return data;
	}

	const items = async <T>(p: Promise<{ items: T[] }>) => (await p).items;

	return {
		health: () => call('GET', '/health', (v): v is { status: string } => isObj(v) && isStr(v.status)),
		version: () => call('GET', '/version', isVersionInfo),

		setupStatus: () =>
			call('GET', '/setup/status', (v): v is { setupRequired: boolean } => isObj(v) && isBool(v.setupRequired)),
		beginSetup: (req: { setupToken: string; accountId: string; displayName?: string; passkeyLabel?: string }) =>
			call('POST', '/setup/begin', isChallenge, req),
		finishSetup: (req: { setupToken: string; ceremonyId: string; credential: CredentialJSON }) =>
			call('POST', '/setup/finish', isSetupResult, req),

		beginLogin: () => call('POST', '/auth/login/begin', isChallenge, {}),
		finishLogin: (req: { ceremonyId: string; credential: CredentialJSON }) =>
			call('POST', '/auth/login/finish', (v): v is { accountId: string } => isObj(v) && isStr(v.accountId), req),
		logout: () => call('POST', '/auth/logout', isAny),
		session: () => call('GET', '/auth/session', isSessionInfo),

		beginPasskey: (label?: string) => call('POST', '/auth/passkeys/begin', isChallenge, label ? { label } : {}),
		finishPasskey: (req: { ceremonyId: string; credential: CredentialJSON }) =>
			call(
				'POST',
				'/auth/passkeys/finish',
				(v): v is { passkey: PasskeyInfo } => isObj(v) && isPasskeyInfo(v.passkey),
				req
			),

		redeemRecoveryCode: (accountId: string, code: string) =>
			call('POST', '/auth/recovery/code', isRecoveryStarted, { accountId, code }),
		redeemRecoveryToken: (accountId: string, token: string) =>
			call('POST', '/auth/recovery/token', isRecoveryStarted, { accountId, token }),

		swarms: () => items(call('GET', '/swarms', listOf(isSwarm))),
		clusters: () => items(call('GET', '/clusters', listOf(isCluster))),
		cluster: (id: string) => call('GET', `/clusters/${encodeURIComponent(id)}`, isCluster),
		operations: () => items(call('GET', '/operations', listOf(isOperation))),
		events: () => items(call('GET', '/events', listOf(isEvent))),

		// --- Endpoints below are NOT in api/openapi.yaml yet. Shapes are the
		// UI's documented assumptions (docs/DESIGN.md "Assumed API contract").
		// 404/503 are shown as "API not available".
		hosts: () => items(call('GET', '/hosts', listOf(isHost))),
		createCluster: (req: CreateClusterRequest) => call('POST', '/clusters', isOperationRef, req),
		replaceNode: (cluster: string, slot: string) =>
			call(
				'POST',
				`/clusters/${encodeURIComponent(cluster)}/nodes/${encodeURIComponent(slot)}/replace`,
				isOperationRef,
				{}
			),
		outageDiagnosis: (cluster: string) =>
			call('GET', `/clusters/${encodeURIComponent(cluster)}/outage-diagnosis`, isDiagnosis),
		backups: () => items(call('GET', '/backups', listOf(isBackupSummary))),
		inspectRestore: (req: { cluster: string; backup?: string }) =>
			call('POST', '/restores/inspect', isObj as Guard<Record<string, unknown>>, req),
		dashboards: () => items(call('GET', '/dashboards', listOf(isDashboard))),
		saveDashboard: (d: DashboardInput, id?: string) =>
			id
				? call('PUT', `/dashboards/${encodeURIComponent(id)}`, isDashboard, d)
				: call('POST', '/dashboards', isDashboard, d),
		deleteDashboard: (id: string) => call('DELETE', `/dashboards/${encodeURIComponent(id)}`, isAny),
		queryMetrics: (req: MetricsQuery) => call('POST', '/metrics/query', isMetricsResult, req),
		notifications: () => items(call('GET', '/notifications', listOf(isNotification))),
		markNotification: (id: string, action: 'read' | 'ack') =>
			call('POST', `/notifications/${encodeURIComponent(id)}/${action}`, isAny, {}),
		notificationSettings: () => call('GET', '/settings/notifications', isNotificationSettings),
		saveNotificationSettings: (s: NotificationSettingsInput) =>
			call('PUT', '/settings/notifications', isNotificationSettings, s)
	};
}

const isRecoveryStarted = (v: unknown): v is { pendingPasskeyEnrollment: true } =>
	isObj(v) && v.pendingPasskeyEnrollment === true;

export type Client = ReturnType<typeof createClient>;

// In development only, VITE_MOCK=1 swaps fetch for an in-memory fixture
// server. The branch is statically false in production builds, so neither the
// mock module nor its fixtures are bundled.
let selectedFetch: FetchFn = (...args) => fetch(...args);
export let mockActive = false;
if (import.meta.env.DEV && import.meta.env.VITE_MOCK === '1') {
	const { mockFetch } = await import('./mock');
	selectedFetch = mockFetch;
	mockActive = true;
}

export const api = createClient(selectedFetch);
