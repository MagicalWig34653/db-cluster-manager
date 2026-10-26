// Pure input rules for the approval-gated cluster operation planner, data-key
// rotation and metastore membership changes. The manager enforces every rule
// again; these only avoid pointless requests.

import type { ClusterOperationInput, ClusterOperationKind } from './api';
import type { MessageKey } from './i18n';

const slotRE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const imageRE = /^[a-z0-9][a-z0-9._\/-]*:\d+\.\d+\.\d+(?:-[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$/;

export const clusterOperationLabel: Record<ClusterOperationKind, MessageKey> = {
	'scale-out': 'cpKindScaleOut',
	'scale-in': 'cpKindScaleIn',
	'rolling-restart': 'cpKindRolling',
	'minor-upgrade': 'cpKindMinor'
};

export const needsSlot = (k: ClusterOperationKind): boolean => k === 'scale-out' || k === 'scale-in';
export const needsImage = (k: ClusterOperationKind): boolean => k === 'minor-upgrade';
export const validSlot = (s: string): boolean => slotRE.test(s.trim());
export const validImage = (s: string): boolean => imageRE.test(s.trim());

export type PlanInputProblem = 'slot' | 'slotExists' | 'slotMissing' | 'image';

/** Returns the first problem of the input or undefined. existing are the current node slots. */
export function planInputProblem(kind: ClusterOperationKind, slot: string, image: string, existing: string[]): PlanInputProblem | undefined {
	if (kind === 'scale-out') {
		if (!validSlot(slot)) return 'slot';
		if (existing.includes(slot.trim())) return 'slotExists';
	}
	if (kind === 'scale-in' && !existing.includes(slot.trim())) return 'slotMissing';
	if (kind === 'minor-upgrade' && !validImage(image)) return 'image';
	return undefined;
}

/** Builds the request; rolling operations must not carry a slot or image. */
export function clusterOperationInput(kind: ClusterOperationKind, slot: string, image: string): ClusterOperationInput {
	return {
		kind,
		...(needsSlot(kind) ? { slot: slot.trim() } : {}),
		...(needsImage(kind) ? { image: image.trim() } : {})
	};
}

/** A scale-in leaving fewer than three nodes cannot keep Galera quorum after one more failure. */
export const scaleInBelowThree = (kind: ClusterOperationKind, nodeCount: number): boolean => kind === 'scale-in' && nodeCount - 1 < 3;

/** Approval of key rotation or a membership change is possible only for the previewed digest. */
export const approvableDigest = (preview: string | undefined, confirmed: boolean, busy: boolean): boolean => !!preview && confirmed && !busy;
