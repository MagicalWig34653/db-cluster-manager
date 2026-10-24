// Pure rules for what the UI offers for an operation, host or restore. The
// manager enforces everything again; these only keep controls honest.

import type { Host, Operation } from './api';

export type ApprovalView =
	| { kind: 'none' }
	| { kind: 'required'; digest: string }
	| { kind: 'approved'; digest: string; by?: string; at?: string };

/** Approval needs a digest requirement in the operation view; nothing is assumed otherwise. */
export function approvalView(op: Pick<Operation, 'approval'>): ApprovalView {
	const a = op.approval;
	if (!a) return { kind: 'none' };
	return a.approvedBy || a.approvedAt
		? { kind: 'approved', digest: a.digest, by: a.approvedBy, at: a.approvedAt }
		: { kind: 'required', digest: a.digest };
}

const finished = (op: Pick<Operation, 'state'>) => op.state === 'succeeded' || op.state === 'failed' || op.state === 'aborted';

export const canApprove = (op: Operation): boolean => approvalView(op).kind === 'required' && !finished(op);
/** Paused operations report state "pending"; one that still waits for approval is resumed by the approval. */
export const canResume = (op: Operation): boolean => op.state === 'pending' && approvalView(op).kind !== 'required';
export const canAbort = (op: Operation): boolean => op.state === 'pending' || op.state === 'running';

/** Hosts an agent can be enrolled on or revoked from. */
export const canEnroll = (h: Pick<Host, 'enrollment'>): boolean => h.enrollment !== 'enrolled';
export const canRevoke = (h: Pick<Host, 'enrollment'>): boolean => h.enrollment === 'enrolled';

/**
 * Hosts offered for an inspection restore: enrolled, ready, active nodes of the
 * cluster's Swarm. Free disk is not observed, so the user must pick one.
 */
export function restoreHosts(hosts: readonly Host[], swarm: string | undefined): Host[] {
	return hosts.filter(
		(h) => h.swarm === swarm && h.enrollment === 'enrolled' && h.state === 'ready' && h.availability === 'active'
	);
}

export interface SwarmForm {
	name: string;
	dockerHost: string;
}

/** Mirrors validDisplayName/validDockerHost of the API. */
export function validateSwarm(f: SwarmForm): ('name' | 'dockerHost')[] {
	const out: ('name' | 'dockerHost')[] = [];
	const name = f.name.trim();
	// eslint-disable-next-line no-control-regex
	if (name === '' || name.length > 64 || /[\u0000-\u001f\u007f]/.test(name)) out.push('name');
	const h = f.dockerHost.trim();
	const okHost = h.length <= 256 && !/\s/.test(h) && (h.startsWith('unix:///') || (h.startsWith('tcp://') && h.length > 'tcp://'.length));
	if (!okHost) out.push('dockerHost');
	return out;
}

/** Summary line counts of an agent deployment. */
export const deploymentFailed = (d: { failed: string[] }): boolean => d.failed.length > 0;
