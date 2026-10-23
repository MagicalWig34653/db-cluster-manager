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

export type NodeRole = 'synced' | 'donor' | 'joining' | 'joined' | 'initialized' | 'unknown';

export interface ClusterNode {
	id: string;
	host?: string;
	state: State;
	/** Galera role from the last fresh observation; unknown when missing or stale (absent from older managers). */
	role?: NodeRole;
	/** Time of the last observation, fresh or not. */
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

/** Mirrors api.OperationApproval: digest binds an approval to the stored operation definition. */
export interface OperationApproval {
	digest: string;
	approvedBy?: string;
	approvedAt?: string;
}

export interface Operation {
	id: string;
	kind: string;
	swarm?: string;
	cluster?: string;
	state: OperationState;
	startedAt?: string;
	endedAt?: string;
	/** Present only for operations that wait for an explicit approval. */
	approval?: OperationApproval;
	/** The runner's short status reason (for example the unmet precondition of a pause). */
	reason?: string;
	/** Step progress as last stored by the leader; absent for operations that report none. */
	progress?: OperationProgress;
}

/** Mirrors api.OperationProgress: step is the first step that has not succeeded. */
export interface OperationProgress {
	stepsDone: number;
	stepsTotal: number;
	step?: string;
	/** Agent command kind; empty for a gate. */
	command?: string;
	/** Host that receives the command. */
	target?: string;
	/** Journal state of the step (pending, running, failed, unknown). */
	state?: string;
	attempts?: number;
	/** Latest unmet precondition or retry reason of the step. */
	waiting?: string;
	polls?: number;
	since?: string;
	updatedAt?: string;
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
	'busy',
	'approval_mismatch',
	'not_leader'
] as const;
export type KnownCode = (typeof knownCodes)[number];
const isKnownCode = (v: unknown): v is KnownCode => isStr(v) && (knownCodes as readonly string[]).includes(v);

const leaderRE = /^[A-Za-z0-9._:[\]-]{1,255}$/;
/** A leader hint is shown only when it looks like host[:port]; anything else is dropped. */
export const leaderHint = (v: unknown): string | undefined => (isStr(v) && leaderRE.test(v) ? v : undefined);

export class ApiError extends Error {
	constructor(
		readonly kind: ErrorKind,
		readonly status: number,
		/** Seconds from Retry-After on 429, if given. */
		readonly retryAfter?: number,
		/** Allowlisted coarse server code (see knownCodes); never arbitrary server text. */
		readonly code?: KnownCode,
		/** Optional leader address of a not_leader answer; only host[:port]-shaped values are kept. */
		readonly leader?: string
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

const digestRE = /^[0-9a-f]{64}$/;
export const isApproval = (v: unknown): v is OperationApproval =>
	isObj(v) && isStr(v.digest) && digestRE.test(v.digest) && optStr(v.approvedBy) && optDate(v.approvedAt);

const nonNegInt = (v: unknown): v is number => isNum(v) && Number.isInteger(v) && v >= 0;
export const isProgress = (v: unknown): v is OperationProgress =>
	isObj(v) &&
	nonNegInt(v.stepsDone) &&
	nonNegInt(v.stepsTotal) &&
	optStr(v.step) &&
	optStr(v.command) &&
	optStr(v.target) &&
	optStr(v.state) &&
	(v.attempts === undefined || nonNegInt(v.attempts)) &&
	optStr(v.waiting) &&
	(v.polls === undefined || nonNegInt(v.polls)) &&
	optDate(v.since) &&
	optDate(v.updatedAt);

export const isOperation: Guard<Operation> = (v): v is Operation =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.kind) &&
	optStr(v.swarm) &&
	optStr(v.cluster) &&
	isOperationState(v.state) &&
	optDate(v.startedAt) &&
	optDate(v.endedAt) &&
	(v.approval === undefined || isApproval(v.approval)) &&
	optStr(v.reason) &&
	(v.progress === undefined || isProgress(v.progress));

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
const isNum = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v);
const optNum = (v: unknown) => v === undefined || isNum(v);

// ---------------------------------------------------------------------------
// Command and monitoring contract (api/openapi.yaml, internal/api). Field
// names follow internal/api, internal/metrics and internal/galera.

export const enrollments = ['discovered', 'enrolled', 'revoked'] as const;
export type Enrollment = (typeof enrollments)[number];

/** Mirrors api.Host. */
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
	/** An agent certificate is recorded for the host; says nothing about reachability. */
	agentIdentity?: boolean;
	/** Set while the host is in guided maintenance. */
	maintenance?: HostMaintenance;
}

/** Mirrors Host.maintenance: replacement on the host is paused while it is set. */
export interface HostMaintenance {
	since: string;
	by: string;
	reason?: string;
	operations: string[];
	/** Every maintenance operation succeeded; the operator may reboot the host. */
	ready: boolean;
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
	safe_to_bootstrap?: boolean;
	empty?: boolean;
}

/** Mirrors galera.Diagnosis (advice only; requires_approval is always true). */
export interface Diagnosis {
	candidate?: string;
	positions: NodePosition[];
	reasons: string[];
	risks?: Risk[];
	requires_approval: boolean;
}

export const missedPolicies = ['skip', 'run-once'] as const;
export type MissedPolicy = (typeof missedPolicies)[number];

/** Mirrors api.BackupPolicy (PUT /clusters/{id}/backup-policy). */
export const backupMethods = ['physical', 'logical'] as const;
export type BackupMethod = (typeof backupMethods)[number];

export interface RetentionPolicy {
	keepLast: number;
	keepDaily?: number;
	keepWeekly?: number;
}

export interface BackupPolicy {
	enabled: boolean;
	/** Five-field cron-like spec. */
	schedule: string;
	/** IANA zone; the manager defaults to UTC. */
	timezone?: string;
	missed?: MissedPolicy;
	method?: BackupMethod;
	/** Logical method only (required there): one export per database. */
	databases?: string[];
	retention?: RetentionPolicy;
}

export interface BackupSummary {
	cluster: string;
	swarm?: string;
	policy?: string;
	backupPolicy?: BackupPolicy;
	lastBackupAt?: string;
	lastBackupId?: string;
	lastVerifiedRestoreAt?: string;
	nextRunAt?: string;
}

/** Mirrors api.InspectionInput. */
export interface InspectionInput {
	cluster: string;
	backup?: string;
	/** Explicitly approved Swarm node; free disk is not observed, so the UI always asks for one. */
	host?: string;
	ttlMinutes?: number;
}

/** Mirrors api.InspectionStarted. The password is returned only in this response. */
export interface InspectionStarted {
	id: string;
	kind: string;
	session: string;
	backup: string;
	host: string;
	service: string;
	port: number;
	expiresAt: string;
	approval: OperationApproval;
	inspectionAccount: string;
	inspectionPassword: string;
}

/** Mirrors api.AgentDeployment. */
export interface AgentDeployment {
	serviceId?: string;
	created: boolean;
	labeled: string[];
	unlabeled: string[];
	failed: string[];
}

export const panelKinds = ['line', 'stat', 'table'] as const;
export type PanelKind = (typeof panelKinds)[number];
export const metricFuncs = ['last', 'avg', 'min', 'max', 'sum', 'count', 'rate', 'quantile'] as const;
export type MetricFunc = (typeof metricFuncs)[number];

/** Mirrors metrics.Panel; x/y/w/h are grid cells on a 24-column grid. */
export interface Panel {
	id: string;
	title: string;
	kind: PanelKind;
	/** Series selector, for example `name{label="v"}`. */
	selector: string;
	func: string;
	windowSeconds: number;
	stepSeconds: number;
	quantile?: number;
	x: number;
	y: number;
	w: number;
	h: number;
}
/** Mirrors api.DashboardInput. */
export interface DashboardInput {
	title: string;
	shared: boolean;
	panels: Panel[];
}
/** Mirrors metrics.Dashboard. Templates and other users' dashboards are read-only. */
export interface Dashboard extends DashboardInput {
	id: string;
	owner: string;
	template?: boolean;
}

export const dashboardIdRE = /^[A-Za-z0-9._-]{1,64}$/;

/** Mirrors api.QueryRequest; start and end are milliseconds since the epoch. */
export interface MetricsQuery {
	selector: string;
	start: number;
	end: number;
	stepSeconds: number;
	windowSeconds?: number;
	func?: string;
	quantile?: number;
}
export const pointStates = ['ok', 'no_data', 'unavailable'] as const;
export type PointState = (typeof pointStates)[number];
/** One step: v is a number only when state is "ok"; t is in milliseconds. */
export interface MetricPoint {
	t: number;
	v: number | null;
	state: PointState;
}
export interface MetricSeries {
	labels: Record<string, string>;
	points: MetricPoint[];
}
/** An interval that could not be read (milliseconds). */
export interface MetricGap {
	from: number;
	to: number;
	reason: string;
}
export interface MetricsResult {
	start: number;
	end: number;
	stepSeconds: number;
	windowSeconds: number;
	series: MetricSeries[];
	gaps: MetricGap[];
}

/** Notification severities include "error" (api/openapi.yaml Notification). */
export const notificationSeverities = ['info', 'warning', 'error', 'critical', 'unknown'] as const;
export type NotificationSeverity = (typeof notificationSeverities)[number];

export interface AppNotification {
	id: string;
	time: string;
	severity: NotificationSeverity;
	kind?: string;
	message: string;
	cluster?: string;
	read: boolean;
	acknowledged: boolean;
}

export const minSeverities = ['info', 'warning', 'error', 'critical'] as const;
export type MinSeverity = (typeof minSeverities)[number];
export type NotificationChannel = 'webhook' | 'email';

export interface NotificationSettings {
	minSeverity?: MinSeverity;
	webhook: { enabled: boolean; url?: string; secretSet: boolean; allowPrivate?: boolean };
	email: {
		enabled: boolean;
		recipients: string[];
		addr?: string;
		from?: string;
		username?: string;
		passwordSet?: boolean;
	};
}
/** The webhook secret and the SMTP password are write-only: sent only when changed, never returned. */
export interface NotificationSettingsInput {
	minSeverity?: MinSeverity;
	webhook: { enabled: boolean; url: string; secret?: string; allowPrivate?: boolean };
	email: { enabled: boolean; recipients: string[]; addr?: string; from?: string; username?: string; password?: string };
}

/** Mirrors api.ClusterDeletion: nothing is removed until the digest is approved. */
export interface ClusterDeletion {
	id: string;
	kind: string;
	approval: OperationApproval;
}

/** Answer of POST /clusters/{id}/outage-recovery: the plan waits for the approval. */
export interface OutageRecoveryStarted {
	id: string;
	kind: string;
	candidate: string;
	approval: OperationApproval;
	diagnosis: Diagnosis;
}

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
	(v.agentIdentity === undefined || isBool(v.agentIdentity)) &&
	(v.maintenance === undefined ||
		(isObj(v.maintenance) &&
			isStr(v.maintenance.since) &&
			isStr(v.maintenance.by) &&
			optStr(v.maintenance.reason) &&
			strArray(v.maintenance.operations) &&
			isBool(v.maintenance.ready)));

export const isOperationRef = (v: unknown): v is { id: string; kind?: string } => isObj(v) && isStr(v.id) && optStr(v.kind);

export const isInspectionStarted = (v: unknown): v is InspectionStarted =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.kind) &&
	isStr(v.session) &&
	isStr(v.backup) &&
	isStr(v.host) &&
	isStr(v.service) &&
	isNum(v.port) &&
	isDate(v.expiresAt) &&
	isApproval(v.approval) &&
	isStr(v.inspectionAccount) &&
	isStr(v.inspectionPassword);

export const isAgentDeployment = (v: unknown): v is AgentDeployment =>
	isObj(v) && optStr(v.serviceId) && isBool(v.created) && strArray(v.labeled) && strArray(v.unlabeled) && strArray(v.failed);

export const isDiagnosis: Guard<Diagnosis> = (v): v is Diagnosis =>
	isObj(v) &&
	optStr(v.candidate) &&
	Array.isArray(v.positions) &&
	v.positions.every((p) => isObj(p) && isStr(p.slot) && isStr(p.source) && optNum(p.seqno) && optStr(p.uuid) && (p.safe_to_bootstrap === undefined || isBool(p.safe_to_bootstrap)) && (p.empty === undefined || isBool(p.empty))) &&
	strArray(v.reasons) &&
	(v.risks === undefined ||
		(Array.isArray(v.risks) && v.risks.every((r) => isObj(r) && isStr(r.kind) && optStr(r.slot) && isStr(r.detail)))) &&
	isBool(v.requires_approval);

export const isBackupPolicy = (v: unknown): v is BackupPolicy =>
	isObj(v) &&
	isBool(v.enabled) &&
	isStr(v.schedule) &&
	optStr(v.timezone) &&
	(v.missed === undefined || oneOf(missedPolicies)(v.missed)) &&
	(v.method === undefined || oneOf(backupMethods)(v.method)) &&
	(v.databases === undefined || strArray(v.databases)) &&
	(v.retention === undefined ||
		(isObj(v.retention) && isNum(v.retention.keepLast) && optNum(v.retention.keepDaily) && optNum(v.retention.keepWeekly)));

export const isBackupSummary: Guard<BackupSummary> = (v): v is BackupSummary =>
	isObj(v) &&
	isStr(v.cluster) &&
	optStr(v.swarm) &&
	optStr(v.policy) &&
	(v.backupPolicy === undefined || isBackupPolicy(v.backupPolicy)) &&
	optDate(v.lastBackupAt) &&
	optStr(v.lastBackupId) &&
	optDate(v.lastVerifiedRestoreAt) &&
	optDate(v.nextRunAt);

const isPanel = (v: unknown): v is Panel =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.title) &&
	oneOf(panelKinds)(v.kind) &&
	isStr(v.selector) &&
	isStr(v.func) &&
	isNum(v.windowSeconds) &&
	isNum(v.stepSeconds) &&
	optNum(v.quantile) &&
	isNum(v.x) &&
	isNum(v.y) &&
	isNum(v.w) &&
	isNum(v.h);

export const isDashboard: Guard<Dashboard> = (v): v is Dashboard =>
	isObj(v) &&
	isStr(v.id) &&
	isStr(v.title) &&
	isStr(v.owner) &&
	isBool(v.shared) &&
	(v.template === undefined || isBool(v.template)) &&
	Array.isArray(v.panels) &&
	v.panels.every(isPanel);

const isMetricPoint = (p: unknown): p is MetricPoint =>
	isObj(p) &&
	isNum(p.t) &&
	oneOf(pointStates)(p.state) &&
	// A value exists exactly for state "ok"; anything else is an explicit gap.
	(p.state === 'ok' ? isNum(p.v) : p.v === null);

export const isMetricsResult: Guard<MetricsResult> = (v): v is MetricsResult =>
	isObj(v) &&
	isNum(v.start) &&
	isNum(v.end) &&
	isNum(v.stepSeconds) &&
	isNum(v.windowSeconds) &&
	Array.isArray(v.series) &&
	v.series.every(
		(s) =>
			isObj(s) &&
			isObj(s.labels) &&
			Object.values(s.labels).every(isStr) &&
			Array.isArray(s.points) &&
			s.points.every(isMetricPoint)
	) &&
	Array.isArray(v.gaps) &&
	v.gaps.every((g) => isObj(g) && isNum(g.from) && isNum(g.to) && isStr(g.reason));

export const isNotification: Guard<AppNotification> = (v): v is AppNotification =>
	isObj(v) &&
	isStr(v.id) &&
	isDate(v.time) &&
	oneOf(notificationSeverities)(v.severity) &&
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
	(v.webhook.allowPrivate === undefined || isBool(v.webhook.allowPrivate)) &&
	(v.minSeverity === undefined || oneOf(minSeverities)(v.minSeverity)) &&
	isObj(v.email) &&
	isBool(v.email.enabled) &&
	strArray(v.email.recipients) &&
	optStr(v.email.addr) &&
	optStr(v.email.from) &&
	optStr(v.email.username) &&
	(v.email.passwordSet === undefined || isBool(v.email.passwordSet));

export const isClusterDeletion = (v: unknown): v is ClusterDeletion =>
	isObj(v) && isStr(v.id) && isStr(v.kind) && isApproval(v.approval);

export const isOutageRecoveryStarted = (v: unknown): v is OutageRecoveryStarted =>
	isObj(v) && isStr(v.id) && isStr(v.kind) && isStr(v.candidate) && isApproval(v.approval) && isDiagnosis(v.diagnosis);


// ---------------------------------------------------------------------------
// Services, image builds, registries, maintenance, drift, keyring
// (api/openapi.yaml; all implemented with fakes only on the manager side).

export const serviceTypes = ['container', 'traefik'] as const;
export type ServiceType = (typeof serviceTypes)[number];

/** Request body of POST /services and POST /services/plan. The spec is type specific and holds no secret values. */
export interface ServiceInput {
	swarm: string;
	name: string;
	type: ServiceType;
	spec: Record<string, unknown>;
}

export interface ServiceInfo {
	id: string;
	swarm: string;
	name: string;
	type: string;
	spec: Record<string, unknown>;
	createdBy?: string;
	createdAt?: string;
	createOperation?: string;
	deleteOperation?: string;
}

export interface PlanStep {
	id: string;
	kind: string;
	slot?: string;
	note?: string;
	conditions?: string[];
}

export interface ServiceArtifact {
	name: string;
	mediaType: string;
	content: string;
}

export interface ServicePlan {
	kind: 'create' | 'delete';
	type: string;
	name: string;
	steps: PlanStep[];
	artifacts: ServiceArtifact[];
	warnings: string[];
	blockers: string[];
	executable: boolean;
	requiresApproval: boolean;
	text: string;
}

export interface ServiceStarted {
	service: string;
	operation: { id: string; kind?: string };
	approval: OperationApproval;
	plan: ServicePlan;
}

export interface MaintenanceCluster {
	cluster: string;
	name: string;
	slots: string[];
	steps: PlanStep[];
	warnings?: string[];
	/** Why this cluster cannot lose its nodes on the host now. */
	refused?: string;
}

export interface MaintenancePreview {
	host: string;
	end: boolean;
	/** No affected cluster refuses. */
	safe: boolean;
	clusters: MaintenanceCluster[];
}

export interface MaintenanceStarted {
	host: string;
	operations: { id: string; kind?: string }[];
	plan: MaintenancePreview;
}

export const driftKinds = [
	'missing-service',
	'service-replaced-outside',
	'foreign-labels',
	'placement-changed',
	'stopped',
	'not-running',
	'unexpected-service'
] as const;
export type DriftKind = (typeof driftKinds)[number];

export interface DriftFinding {
	kind: DriftKind;
	slot?: string;
	service?: string;
	detail: string;
	/** Proposed manual next step; nothing is changed automatically. */
	action: string;
}

export interface DriftReport {
	cluster: string;
	checkedAt: string;
	inSync: boolean;
	findings: DriftFinding[];
}

export interface ReplicaKeyStatus {
	id: string;
	address?: string;
	leader: boolean;
	/** Fingerprint of the registered public identity. */
	publicKeyId?: string;
	registeredBy?: string;
	registeredAt?: string;
	hasCurrentKey: boolean;
}

export interface KeyringStatus {
	/** ID of the current data key (not the key). */
	currentKey: string;
	/** The current key has a backup recovery key envelope. */
	recoverable: boolean;
	replicas: ReplicaKeyStatus[];
}

export interface ImageBuildSpec {
	name: string;
	nginx: {
		version: string;
		baseDigest: string;
		options?: Record<string, unknown>;
		customSnippet?: string;
	};
	php?: { version?: string; extensions?: string[]; ini?: Record<string, string> };
	extraPackages?: string[];
	platforms?: string[];
}

export interface ImageBuildRequest {
	spec: ImageBuildSpec;
	target: { registry: string; repository: string; tag: string };
	/** Digest of the reviewed plan; starts the build. */
	approvalDigest?: string;
}

export interface ImageBuildPlan {
	specHash: string;
	contextHash: string;
	digest: string;
	baseImage?: string;
	platforms?: string[];
	reference?: string;
	dockerfile: string;
	files?: { path?: string; mode?: number; content?: string }[];
	steps: { order?: number; title?: string; detail?: string }[];
	warnings: string[];
	explanation: string;
}

export const buildStatuses = ['running', 'succeeded', 'failed'] as const;
export type BuildStatus = (typeof buildStatuses)[number];

export interface ImageBuildJob {
	id: string;
	name?: string;
	status: BuildStatus;
	specHash: string;
	contextHash: string;
	planDigest: string;
	reference: string;
	platforms?: string[];
	baseDigests?: string[];
	imageDigest?: string;
	sbom: { status?: string; ref?: string };
	/** Redacted. */
	error?: string;
	requestedBy?: string;
	createdAt?: string;
	finishedAt?: string;
}

export interface ImageBuildResult {
	plan?: ImageBuildPlan;
	job?: ImageBuildJob;
}

export const registryKinds = ['ghcr', 'dockerhub', 'generic', 'managed'] as const;
export type RegistryKind = (typeof registryKinds)[number];

export interface Registry {
	id: string;
	name: string;
	kind: RegistryKind;
	endpoint?: string;
	username?: string;
	/** Only says that a credential is stored; it is never returned. */
	credentialSet: boolean;
	createdBy?: string;
}

/** The password is write-only: sent once, never returned or kept in the UI after the request. */
export interface ExternalRegistryInput {
	id: string;
	name: string;
	kind: Exclude<RegistryKind, 'managed'>;
	endpoint?: string;
	username?: string;
	password?: string;
}

const isPlanStep = (v: unknown): v is PlanStep =>
	isObj(v) && isStr(v.id) && isStr(v.kind) && optStr(v.slot) && optStr(v.note) && (v.conditions === undefined || strArray(v.conditions));

export const isServiceInfo: Guard<ServiceInfo> = (v): v is ServiceInfo =>
	isObj(v) && isStr(v.id) && isStr(v.swarm) && isStr(v.name) && isStr(v.type) && isObj(v.spec) && optStr(v.createdBy) && optDate(v.createdAt) && optStr(v.createOperation) && optStr(v.deleteOperation);

export const isServicePlan = (v: unknown): v is ServicePlan =>
	isObj(v) &&
	(v.kind === 'create' || v.kind === 'delete') &&
	isStr(v.type) &&
	isStr(v.name) &&
	Array.isArray(v.steps) &&
	v.steps.every(isPlanStep) &&
	Array.isArray(v.artifacts) &&
	v.artifacts.every((a) => isObj(a) && isStr(a.name) && isStr(a.mediaType) && isStr(a.content)) &&
	strArray(v.warnings) &&
	strArray(v.blockers) &&
	isBool(v.executable) &&
	isBool(v.requiresApproval) &&
	isStr(v.text);

export const isServiceStarted = (v: unknown): v is ServiceStarted =>
	isObj(v) && isStr(v.service) && isOperationRef(v.operation) && isApproval(v.approval) && isServicePlan(v.plan);

const isMaintenanceCluster = (v: unknown): v is MaintenanceCluster =>
	isObj(v) &&
	isStr(v.cluster) &&
	isStr(v.name) &&
	strArray(v.slots) &&
	Array.isArray(v.steps) &&
	v.steps.every(isPlanStep) &&
	(v.warnings === undefined || strArray(v.warnings)) &&
	optStr(v.refused);

export const isMaintenancePreview = (v: unknown): v is MaintenancePreview =>
	isObj(v) && isStr(v.host) && isBool(v.end) && isBool(v.safe) && Array.isArray(v.clusters) && v.clusters.every(isMaintenanceCluster);

export const isMaintenanceStarted = (v: unknown): v is MaintenanceStarted =>
	isObj(v) && isStr(v.host) && Array.isArray(v.operations) && v.operations.every(isOperationRef) && isMaintenancePreview(v.plan);

export const isDriftReport = (v: unknown): v is DriftReport =>
	isObj(v) &&
	isStr(v.cluster) &&
	isDate(v.checkedAt) &&
	isBool(v.inSync) &&
	Array.isArray(v.findings) &&
	v.findings.every((f) => isObj(f) && oneOf(driftKinds)(f.kind) && optStr(f.slot) && optStr(f.service) && isStr(f.detail) && isStr(f.action));

const isReplica = (r: unknown): r is ReplicaKeyStatus =>
	isObj(r) && isStr(r.id) && optStr(r.address) && isBool(r.leader) && optStr(r.publicKeyId) && optStr(r.registeredBy) && optDate(r.registeredAt) && isBool(r.hasCurrentKey);

export const isKeyringStatus = (v: unknown): v is KeyringStatus =>
	isObj(v) && isStr(v.currentKey) && isBool(v.recoverable) && Array.isArray(v.replicas) && v.replicas.every(isReplica);

export const isReplicaKeyStatus = isReplica;

const isImageBuildPlan = (v: unknown): v is ImageBuildPlan =>
	isObj(v) &&
	isStr(v.specHash) &&
	isStr(v.contextHash) &&
	isStr(v.digest) &&
	optStr(v.baseImage) &&
	optStr(v.reference) &&
	isStr(v.dockerfile) &&
	(v.files === undefined || (Array.isArray(v.files) && v.files.every((f) => isObj(f) && optStr(f.path) && optStr(f.content)))) &&
	Array.isArray(v.steps) &&
	v.steps.every((s) => isObj(s) && optStr(s.title) && optStr(s.detail)) &&
	strArray(v.warnings) &&
	isStr(v.explanation);

export const isImageBuildJob: Guard<ImageBuildJob> = (v): v is ImageBuildJob =>
	isObj(v) &&
	isStr(v.id) &&
	optStr(v.name) &&
	oneOf(buildStatuses)(v.status) &&
	isStr(v.specHash) &&
	isStr(v.contextHash) &&
	isStr(v.planDigest) &&
	isStr(v.reference) &&
	(v.platforms === undefined || strArray(v.platforms)) &&
	(v.baseDigests === undefined || strArray(v.baseDigests)) &&
	optStr(v.imageDigest) &&
	isObj(v.sbom) &&
	optStr(v.error) &&
	optStr(v.requestedBy) &&
	optDate(v.createdAt) &&
	optDate(v.finishedAt);

export const isImageBuildResult = (v: unknown): v is ImageBuildResult =>
	isObj(v) && (v.plan === undefined || isImageBuildPlan(v.plan)) && (v.job === undefined || isImageBuildJob(v.job));

export const isRegistry: Guard<Registry> = (v): v is Registry =>
	isObj(v) && isStr(v.id) && isStr(v.name) && oneOf(registryKinds)(v.kind) && optStr(v.endpoint) && optStr(v.username) && isBool(v.credentialSet) && optStr(v.createdBy);

const listOfArray =
	<T>(item: (v: unknown) => v is T) =>
	(v: unknown): v is T[] =>
		Array.isArray(v) && v.every(item);

// ---------------------------------------------------------------------------
// Client

export type FetchFn = typeof fetch;

/** A fresh ID for a new dashboard; always matches dashboardIdRE. */
export function newDashboardId(): string {
	return `d-${crypto.randomUUID()}`;
}

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
			let leader: string | undefined;
			try {
				const b: unknown = await res.json();
				if (isObj(b) && isStr(b.error)) code = b.error;
				if (isObj(b)) leader = leaderHint(b.leader);
			} catch {
				// non-JSON error body: classify by status only
			}
			const ra = Number(res.headers.get('Retry-After'));
			throw new ApiError(
				classify(res.status, code),
				res.status,
				Number.isFinite(ra) && ra > 0 ? ra : undefined,
				isKnownCode(code) ? code : undefined,
				code === 'not_leader' ? leader : undefined
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

		// --- Command and monitoring endpoints (api/openapi.yaml). 404/503
		// without a coarse code are shown as "API not available".
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
		/** Plans a deletion; nothing is removed before approveOperation with the returned digest. */
		deleteCluster: (id: string) => call('DELETE', `/clusters/${encodeURIComponent(id)}`, isClusterDeletion),
		/** Also runs wsrep-recover on nodes without a saved seqno (operate permission); starts no service. */
		recoverOutagePositions: (cluster: string) =>
			call('POST', `/clusters/${encodeURIComponent(cluster)}/outage-diagnosis`, isDiagnosis, {}),
		requestOutageRecovery: (cluster: string, candidate: string) =>
			call('POST', `/clusters/${encodeURIComponent(cluster)}/outage-recovery`, isOutageRecoveryStarted, { candidate }),
		startBackup: (cluster: string, opts: { method?: BackupMethod; databases?: string[] } = {}) =>
			call('POST', `/clusters/${encodeURIComponent(cluster)}/backups`, isOperationRef, opts),
		setBackupPolicy: (cluster: string, p: BackupPolicy) =>
			call('PUT', `/clusters/${encodeURIComponent(cluster)}/backup-policy`, isBackupSummary, p),
		testNotification: (channel: NotificationChannel) =>
			call(
				'POST',
				'/settings/notifications/test',
				(v): v is { channel: string; delivered: boolean } => isObj(v) && isStr(v.channel) && isBool(v.delivered),
				{ channel }
			),
		backups: () => items(call('GET', '/backups', listOf(isBackupSummary))),
		inspectRestore: (req: InspectionInput) => call('POST', '/restores/inspect', isInspectionStarted, req),
		dashboards: () => items(call('GET', '/dashboards', listOf(isDashboard))),
		/** Updates with PUT /dashboards/{id}; without an id creates with POST /dashboards/{newId} (client-chosen ID). */
		saveDashboard: (d: DashboardInput, id?: string) =>
			id
				? call('PUT', `/dashboards/${encodeURIComponent(id)}`, isDashboard, d)
				: call('POST', `/dashboards/${encodeURIComponent(newDashboardId())}`, isDashboard, d),
		deleteDashboard: (id: string) => call('DELETE', `/dashboards/${encodeURIComponent(id)}`, isAny),
		queryMetrics: (req: MetricsQuery) => call('POST', '/metrics/query', isMetricsResult, req),
		notifications: () => items(call('GET', '/notifications', listOf(isNotification))),
		markNotification: (id: string, action: 'read' | 'ack') =>
			call('POST', `/notifications/${encodeURIComponent(id)}/${action}`, isAny, {}),
		connectSwarm: (req: { name: string; dockerHost: string }) => call('POST', '/swarms', isSwarm, req),
		syncSwarm: (id: string) => call('POST', `/swarms/${encodeURIComponent(id)}/sync`, isAny, {}),
		deployAgents: (id: string) =>
			call('POST', `/swarms/${encodeURIComponent(id)}/agents/deploy`, isAgentDeployment, {}),
		/** Enroll or revoke the agent of a host (the path takes the Swarm node ID). */
		setEnrollment: (nodeId: string, action: 'enroll' | 'revoke') =>
			call('POST', `/hosts/${encodeURIComponent(nodeId)}/${action}`, isHost, {}),
		resumeOperation: (id: string) => call('POST', `/operations/${encodeURIComponent(id)}/resume`, isOperationRef, {}),
		abortOperation: (id: string) => call('POST', `/operations/${encodeURIComponent(id)}/abort`, isOperationRef, {}),
		/** The digest is the one of the displayed approval; the manager refuses a mismatch. */
		approveOperation: (id: string, digest: string) =>
			call('POST', `/operations/${encodeURIComponent(id)}/approve`, isApproval, { digest }),
		// --- Services, image builds, registries, maintenance, drift, keyring.
		services: () => items(call('GET', '/services', listOf(isServiceInfo))),
		previewService: (input: ServiceInput) => call('POST', '/services/plan', isServicePlan, input),
		createService: (input: ServiceInput) => call('POST', '/services', isServiceStarted, input),
		previewServiceDelete: (id: string, removeVolumes: boolean) =>
			call('GET', `/services/${encodeURIComponent(id)}/delete-plan${removeVolumes ? '?removeVolumes=true' : ''}`, isServicePlan),
		deleteService: (id: string, removeVolumes: boolean) =>
			call('DELETE', `/services/${encodeURIComponent(id)}`, isServiceStarted, removeVolumes ? { removeVolumes: true } : undefined),
		imageBuilds: () => call('GET', '/image-builds', listOfArray(isImageBuildJob)),
		/** Without approvalDigest the answer carries the plan only; nothing is built. */
		planImageBuild: (req: ImageBuildRequest) => call('POST', '/image-builds', isImageBuildResult, req),
		startImageBuild: (req: ImageBuildRequest, approvalDigest: string) =>
			call('POST', '/image-builds', isImageBuildResult, { ...req, approvalDigest }),
		registries: () => call('GET', '/registries', listOfArray(isRegistry)),
		createRegistry: (input: ExternalRegistryInput) => call('POST', '/registries', isRegistry, input),
		maintenancePlan: (nodeId: string, end: boolean) =>
			call('GET', `/hosts/${encodeURIComponent(nodeId)}/maintenance-plan${end ? '?end=true' : ''}`, isMaintenancePreview),
		enterMaintenance: (nodeId: string, reason?: string) =>
			call('POST', `/hosts/${encodeURIComponent(nodeId)}/maintenance`, isMaintenanceStarted, reason ? { reason } : {}),
		leaveMaintenance: (nodeId: string) => call('DELETE', `/hosts/${encodeURIComponent(nodeId)}/maintenance`, isMaintenanceStarted),
		clusterDrift: (id: string) => call('GET', `/clusters/${encodeURIComponent(id)}/drift`, isDriftReport),
		keyring: () => call('GET', '/settings/replicas', isKeyringStatus),
		registerReplicaKey: (replica: string, publicKey: string) =>
			call('PUT', `/settings/replicas/${encodeURIComponent(replica)}/data-key-recipient`, isReplicaKeyStatus, { publicKey }),
		revokeReplicaCertificates: (replica: string) =>
			call(
				'POST',
				`/settings/replicas/${encodeURIComponent(replica)}/revoke-certificates`,
				(v): v is { replica: string; revoked: number } => isObj(v) && isStr(v.replica) && isNum(v.revoked),
				{}
			),
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
