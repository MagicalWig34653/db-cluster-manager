import type { ImageUpdatePlan, ImageUpdatePolicy } from './api';

export type ImageUpdateAction = 'approve' | 'apply' | 'decline' | 'rollback';

/** A missing policy means manual (DELETE on the policy route has the same result). */
export function policySummary(p: ImageUpdatePolicy | undefined): string {
	if (!p?.mode || p.mode === 'manual') return 'manual';
	const windows = (p.windows ?? []).map((w) => `${w.days?.length ? `${w.days.join(',')} ` : ''}${w.start} +${w.durationMinutes} min`);
	return [p.mode, p.track, ...windows].filter(Boolean).join(' · ');
}

/**
 * The actions worth offering. Approval is hidden for plans the API refuses with
 * policy_conflict (Galera plans use the planner; foreign services are report-only).
 */
export function planActions(p: Pick<ImageUpdatePlan, 'state' | 'route' | 'kind'>): ImageUpdateAction[] {
	if (p.state === 'proposed') return p.route === 'service-update' ? ['approve', 'decline'] : ['decline'];
	if (p.state === 'approved') return ['apply', 'decline'];
	if (p.state === 'succeeded' && p.kind !== 'rollback') return ['rollback'];
	return [];
}
