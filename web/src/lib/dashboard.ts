// Pure helpers for dashboard editing; the rules mirror metrics.Dashboard.Validate
// so obvious mistakes are caught before the request. The manager stays the
// authority (it also parses the selector), and its answers are shown as errors.

import { metricFuncs, type Dashboard, type MetricFunc, type Panel } from './api';
import type { MessageKey } from './i18n';

export const GRID_COLUMNS = 24;
const MAX_PANELS = 100;

/** A new panel placed below existing ones, two per row on the 24-column grid. */
export function defaultPanel(index: number, id: string): Panel {
	return {
		id,
		title: '',
		kind: 'line',
		selector: '',
		func: 'last',
		windowSeconds: 60,
		stepSeconds: 60,
		x: (index % 2) * 12,
		y: Math.floor(index / 2) * 6,
		w: 12,
		h: 6
	};
}

export type DashboardIssue = { field: 'title' | 'panels' | 'panelTitle' | 'selector' | 'func' | 'step' | 'window' | 'quantile'; panel?: number; key: MessageKey };

export function validateDashboard(d: { title: string; panels: readonly Panel[] }): DashboardIssue[] {
	const out: DashboardIssue[] = [];
	if (d.title.trim() === '' || new TextEncoder().encode(d.title).length > 200) out.push({ field: 'title', key: 'valDashTitle' });
	if (d.panels.length > MAX_PANELS) out.push({ field: 'panels', key: 'valDashPanels' });
	d.panels.forEach((p, i) => {
		if (p.title.trim() === '' || new TextEncoder().encode(p.title).length > 200) out.push({ field: 'panelTitle', panel: i, key: 'valPanelTitle' });
		if (p.selector.trim() === '' || p.selector.length > 1024) out.push({ field: 'selector', panel: i, key: 'valSelector' });
		if (!(metricFuncs as readonly string[]).includes(p.func)) out.push({ field: 'func', panel: i, key: 'valFunc' });
		if (!Number.isInteger(p.stepSeconds) || p.stepSeconds <= 0) out.push({ field: 'step', panel: i, key: 'valStep' });
		if (!Number.isInteger(p.windowSeconds) || p.windowSeconds < 0) out.push({ field: 'window', panel: i, key: 'valWindow' });
		if (p.func === 'quantile' && !((p.quantile ?? 0) >= 0 && (p.quantile ?? 0) <= 1)) out.push({ field: 'quantile', panel: i, key: 'valQuantile' });
	});
	return out;
}

/** Panels in reading order (row, then column) for a responsive grid. */
export const sortPanels = (panels: readonly Panel[]): Panel[] =>
	[...panels].sort((a, b) => a.y - b.y || a.x - b.x);

/** Column span of a panel, clamped to the grid. */
export const spanOf = (p: Panel): number => Math.min(Math.max(Math.round(p.w), 1), GRID_COLUMNS);

/** Only the owner may change a dashboard, and never a template; sharing grants viewing only. */
export function canEditDashboard(d: Pick<Dashboard, 'owner' | 'template'>, accountId: string | undefined): boolean {
	return !d.template && accountId !== undefined && d.owner === accountId;
}

export const isMetricFunc = (v: string): v is MetricFunc => (metricFuncs as readonly string[]).includes(v);
