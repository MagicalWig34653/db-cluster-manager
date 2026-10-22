// Pure geometry for the hand-written SVG line chart. Missing data is never
// interpolated: points that are not "ok" (no_data, unavailable) and holes
// larger than 1.5 steps split the line and are reported as typed gap ranges so
// the chart can show them explicitly. All times are milliseconds, matching the
// manager's metrics API.

import type { MetricGap, MetricSeries } from './api';

export interface Pt {
	x: number;
	y: number;
}
/** no_data: readable but empty; unavailable: cannot be read; missing: the step is absent from the answer. */
export type GapKind = 'no_data' | 'unavailable' | 'missing';
const rank: Record<GapKind, number> = { missing: 0, no_data: 1, unavailable: 2 };
const worse = (a: GapKind | undefined, b: GapKind): GapKind => (a === undefined || rank[b] > rank[a] ? b : a);

export interface GapRange {
	x1: number;
	x2: number;
	kind: GapKind;
}
export interface ChartInput {
	series: MetricSeries;
	/** Intervals the manager reported as unreadable (milliseconds). */
	gaps?: MetricGap[];
	from: number; // unix milliseconds
	to: number;
	step: number; // milliseconds
	width: number;
	height: number;
	pad: { l: number; r: number; t: number; b: number };
}
export interface Chart {
	/** Connected runs of real points. A run of one point is drawn as a dot. */
	runs: Pt[][];
	gaps: GapRange[];
	yTicks: { y: number; value: number }[];
	xTicks: { x: number; time: number }[];
	yMin: number;
	yMax: number;
	hasData: boolean;
	valuePoints: number;
}

/** Round a range to 4-6 "nice" tick values. */
export function niceTicks(min: number, max: number, count = 4): number[] {
	if (!Number.isFinite(min) || !Number.isFinite(max)) return [];
	if (min === max) {
		const d = Math.abs(min) > 0 ? Math.abs(min) * 0.1 : 1;
		min -= d;
		max += d;
	}
	const raw = (max - min) / count;
	const mag = 10 ** Math.floor(Math.log10(raw));
	const norm = raw / mag;
	const stepSize = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
	const out: number[] = [];
	for (let v = Math.ceil(min / stepSize) * stepSize; v <= max + stepSize * 1e-9; v += stepSize) {
		out.push(Math.round(v / stepSize) * stepSize);
	}
	return out;
}

export function buildChart(input: ChartInput): Chart {
	const { series, from, to, step, width, height, pad } = input;
	const span = Math.max(to - from, 1);
	const innerW = Math.max(width - pad.l - pad.r, 1);
	const innerH = Math.max(height - pad.t - pad.b, 1);
	const xOf = (t: number) => pad.l + ((t - from) / span) * innerW;
	const xTicks = [0, 1, 2, 3, 4].map((i) => ({ x: xOf(from + (span * i) / 4), time: from + (span * i) / 4 }));

	const pts = series.points.filter((p) => p.t >= from && p.t <= to).sort((a, b) => a.t - b.t);
	// Only state "ok" carries a value; the type guard also protects against a null slipping through.
	const real = pts.filter((p): p is typeof p & { v: number } => p.state === 'ok' && p.v !== null);

	// Reported unreadable intervals, clipped to the range; they are always unavailable.
	const reported: GapRange[] = (input.gaps ?? [])
		.filter((g) => g.to > from && g.from < to && g.to > g.from)
		.map((g) => ({ x1: xOf(Math.max(g.from, from)), x2: xOf(Math.min(g.to, to)), kind: 'unavailable' as const }));

	if (real.length === 0) {
		// Everything is a gap; the strongest reason wins so "unavailable" is never softened to "no data".
		const kind = pts.reduce<GapKind | undefined>((k, p) => (p.state === 'ok' ? k : worse(k, p.state)), undefined);
		const hasReported = reported.length > 0;
		return {
			runs: [],
			gaps: [{ x1: xOf(from), x2: xOf(to), kind: hasReported ? 'unavailable' : (kind ?? 'missing') }],
			yTicks: [],
			xTicks,
			yMin: 0,
			yMax: 0,
			hasData: false,
			valuePoints: 0
		};
	}

	let lo = Math.min(...real.map((p) => p.v));
	let hi = Math.max(...real.map((p) => p.v));
	const ticks = niceTicks(lo, hi);
	if (ticks.length > 0) {
		lo = Math.min(lo, ticks[0]);
		hi = Math.max(hi, ticks[ticks.length - 1]);
	}
	if (lo === hi) {
		lo -= 1;
		hi += 1;
	}
	const yOf = (v: number) => pad.t + (1 - (v - lo) / (hi - lo)) * innerH;

	const maxHole = step * 1.5;
	const runs: Pt[][] = [];
	const gaps: GapRange[] = [];
	let run: Pt[] = [];
	let prev: number | undefined; // time of the previous real point
	let pending: GapKind | undefined; // worst non-ok state seen since the previous real point
	const flush = () => {
		if (run.length) runs.push(run);
		run = [];
	};
	let i = 0;
	for (const p of pts) {
		if (p.state !== 'ok') {
			pending = worse(pending, p.state);
			continue;
		}
		if (p.v === null) continue;
		if (prev === undefined) {
			// Leading gap.
			if (pending !== undefined || p.t - from > maxHole) gaps.push({ x1: xOf(from), x2: xOf(p.t), kind: pending ?? 'missing' });
		} else if (pending !== undefined || p.t - prev > maxHole) {
			flush();
			gaps.push({ x1: xOf(prev), x2: xOf(p.t), kind: pending ?? 'missing' });
		}
		pending = undefined;
		run.push({ x: xOf(p.t), y: yOf(p.v) });
		prev = p.t;
		i++;
	}
	flush();
	// Trailing gap: the newest steps are not "ok" or the data simply ends early.
	if (prev !== undefined && (pending !== undefined || to - prev > maxHole)) {
		gaps.push({ x1: xOf(prev), x2: xOf(to), kind: pending ?? 'missing' });
	}
	gaps.push(...reported);

	return {
		runs,
		gaps,
		yTicks: ticks.filter((v) => v >= lo && v <= hi).map((v) => ({ y: yOf(v), value: v })),
		xTicks,
		yMin: lo,
		yMax: hi,
		hasData: true,
		valuePoints: i
	};
}

export const pathOf = (run: Pt[]) =>
	run.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join(' ');

/**
 * What a stat tile or table row may claim about the current value of one
 * series. Only a recent "ok" point is a value; a newer no_data/unavailable
 * step makes the current value unknown, and an old "ok" point is stale.
 */
export type CurrentValue =
	| { kind: 'value'; value: number; time: number }
	| { kind: 'stale'; value: number; time: number; ageMs: number }
	| { kind: 'unavailable' }
	| { kind: 'no_data' };

export function currentValue(series: MetricSeries, end: number, stepMs: number): CurrentValue {
	let newest: MetricSeries['points'][number] | undefined;
	let newestOk: (typeof series.points)[number] & { v: number } | undefined;
	let anyUnavailable = false;
	for (const p of series.points) {
		if (!newest || p.t > newest.t) newest = p;
		if (p.state === 'ok' && p.v !== null && (!newestOk || p.t > newestOk.t)) newestOk = p as typeof p & { v: number };
		if (p.state === 'unavailable') anyUnavailable = true;
	}
	if (!newest) return { kind: 'no_data' };
	if (!newestOk) return { kind: anyUnavailable ? 'unavailable' : 'no_data' };
	if (newest.t > newestOk.t) return { kind: newest.state === 'unavailable' ? 'unavailable' : 'no_data' };
	const ageMs = end - newestOk.t;
	return ageMs <= stepMs * 3
		? { kind: 'value', value: newestOk.v, time: newestOk.t }
		: { kind: 'stale', value: newestOk.v, time: newestOk.t, ageMs };
}

/** Compact text for series labels, for example `host=a, slot=n1`; empty when there are none. */
export function labelText(labels: Record<string, string>): string {
	return Object.entries(labels)
		.filter(([k]) => k !== 'name')
		.sort(([a], [b]) => (a < b ? -1 : 1))
		.map(([k, v]) => `${k}=${v}`)
		.join(', ');
}
