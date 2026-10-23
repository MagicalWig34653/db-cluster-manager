// Pure rules for host maintenance, cluster drift and the replica keyring
// settings. The manager enforces everything again.

import type { DriftFinding, DriftKind, DriftReport, Host, KeyringStatus, MaintenancePreview, ReplicaKeyStatus } from './api';
import type { MessageKey } from './i18n';

// ---------------------------------------------------------------------------
// Host maintenance

export type MaintenanceState = 'none' | 'inProgress' | 'ready';

export function maintenanceState(h: Pick<Host, 'maintenance'>): MaintenanceState {
	if (!h.maintenance) return 'none';
	return h.maintenance.ready ? 'ready' : 'inProgress';
}

/** Only an enrolled host that is not already in maintenance can enter it. */
export const canEnterMaintenance = (h: Pick<Host, 'enrollment' | 'maintenance'>): boolean => h.enrollment === 'enrolled' && !h.maintenance;
export const canLeaveMaintenance = (h: Pick<Host, 'maintenance'>): boolean => !!h.maintenance;

export interface MaintenanceView {
	/** The manager would start the operations (no cluster refuses). */
	canStart: boolean;
	refused: { cluster: string; name: string; why: string }[];
	stepCount: number;
	warnings: string[];
	/** Hosts without any cluster node: nothing to stop. */
	empty: boolean;
}

export function maintenanceView(p: MaintenancePreview): MaintenanceView {
	const refused = p.clusters.filter((c) => c.refused).map((c) => ({ cluster: c.cluster, name: c.name, why: c.refused as string }));
	return {
		canStart: p.safe && refused.length === 0,
		refused,
		stepCount: p.clusters.reduce((n, c) => n + c.steps.length, 0),
		warnings: p.clusters.flatMap((c) => c.warnings ?? []),
		empty: p.clusters.length === 0
	};
}

// ---------------------------------------------------------------------------
// Drift

export const driftLabel: Record<DriftKind, MessageKey> = {
	'missing-service': 'driftMissing',
	'service-replaced-outside': 'driftReplaced',
	'foreign-labels': 'driftLabels',
	'placement-changed': 'driftPlacement',
	stopped: 'driftStopped',
	'not-running': 'driftNotRunning',
	'unexpected-service': 'driftUnexpected'
};

/** Findings that mean a node service is gone or not serving (shown first). */
const severe: DriftKind[] = ['missing-service', 'not-running', 'service-replaced-outside'];

export function driftOrdered(findings: readonly DriftFinding[]): DriftFinding[] {
	const rank = (f: DriftFinding) => (severe.includes(f.kind) ? 0 : 1);
	return findings.map((f, i) => ({ f, i })).sort((a, b) => rank(a.f) - rank(b.f) || a.i - b.i).map((x) => x.f);
}

export interface DriftSummary {
	/** inSync is only believed when the report says so and lists no finding. */
	inSync: boolean;
	count: number;
	severe: number;
}

export function driftSummary(r: DriftReport): DriftSummary {
	return { inSync: r.inSync && r.findings.length === 0, count: r.findings.length, severe: r.findings.filter((f) => severe.includes(f.kind)).length };
}

// ---------------------------------------------------------------------------
// Replica keyring

/** dbcmpk1:<base64url X25519 public key, 32 bytes = 43 characters without padding>. */
export const replicaKeyRE = /^dbcmpk1:[A-Za-z0-9_-]{43}$/;
export const validReplicaKey = (s: string): boolean => replicaKeyRE.test(s.trim());

export type ReplicaState = 'ready' | 'noKey' | 'unregistered';

export function replicaState(r: ReplicaKeyStatus): ReplicaState {
	if (!r.publicKeyId) return 'unregistered';
	return r.hasCurrentKey ? 'ready' : 'noKey';
}

export interface KeyringSummary {
	replicas: number;
	ready: number;
	/** Replicas that cannot unseal secrets and so cannot act as leader. */
	notReady: number;
	recoverable: boolean;
}

export function keyringSummary(k: KeyringStatus): KeyringSummary {
	const ready = k.replicas.filter((r) => replicaState(r) === 'ready').length;
	return { replicas: k.replicas.length, ready, notReady: k.replicas.length - ready, recoverable: k.recoverable };
}

/** Leader first, then by ID, so the table does not jump between refreshes. */
export const replicasOrdered = (rs: readonly ReplicaKeyStatus[]): ReplicaKeyStatus[] =>
	[...rs].sort((a, b) => Number(b.leader) - Number(a.leader) || a.id.localeCompare(b.id));
