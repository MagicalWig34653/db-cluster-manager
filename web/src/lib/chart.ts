// Pure geometry for the hand-written SVG line chart. Missing data is never
// interpolated: null values and holes larger than 1.5 steps split the line and
// are reported as gap ranges so the chart can show them explicitly.

import type { MetricSeries } from './api';

export interface Pt {
	x: number;
	y: number;
}
export interface GapRange {
	x1: number;
	x2: number;
}
export interface ChartInput {
	series: MetricSeries;
	from: number; // unix seconds
	to: number;
	step: number; // seconds
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

	const pts = series.points
		.filter(([t]) => t >= from && t <= to)
		.sort((a, b) => a[0] - b[0]);
	const real = pts.filter((p): p is [number, number] => p[1] !== null);
	const empty: Chart = {
		runs: [],
		gaps: [{ x1: xOf(from), x2: xOf(to) }],
		yTicks: [],
		xTicks: [],
		yMin: 0,
		yMax: 0,
		hasData: false,
		valuePoints: 0
	};
	const xTicks = [0, 1, 2, 3, 4].map((i) => ({ x: xOf(from + (span * i) / 4), time: from + (span * i) / 4 }));
	if (real.length === 0) return { ...empty, xTicks };

	let lo = Math.min(...real.map((p) => p[1]));
	let hi = Math.max(...real.map((p) => p[1]));
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
	let prev: number | undefined; // time of previous real point
	const flush = () => {
		if (run.length) runs.push(run);
		run = [];
	};
	// Leading gap.
	if (real[0][0] - from > maxHole) gaps.push({ x1: xOf(from), x2: xOf(real[0][0]) });
	for (const [t, v] of real) {
		if (prev !== undefined && t - prev > maxHole) {
			flush();
			gaps.push({ x1: xOf(prev), x2: xOf(t) });
		}
		run.push({ x: xOf(t), y: yOf(v) });
		prev = t;
	}
	flush();
	if (prev !== undefined && to - prev > maxHole) gaps.push({ x1: xOf(prev), x2: xOf(to) });

	return {
		runs,
		gaps,
		yTicks: ticks.filter((v) => v >= lo && v <= hi).map((v) => ({ y: yOf(v), value: v })),
		xTicks,
		yMin: lo,
		yMax: hi,
		hasData: true,
		valuePoints: real.length
	};
}

export const pathOf = (run: Pt[]) =>
	run.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join(' ');

/** Latest real value of a series, with how old it is; undefined when there is none. */
export function latest(series: MetricSeries[]): { value: number; time: number } | undefined {
	let best: { value: number; time: number } | undefined;
	for (const s of series)
		for (const [t, v] of s.points) if (v !== null && (!best || t > best.time)) best = { value: v, time: t };
	return best;
}
