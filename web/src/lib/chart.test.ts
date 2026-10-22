import { describe, expect, it } from 'vitest';
import type { MetricPoint, MetricSeries } from './api';
import { buildChart, currentValue, labelText, niceTicks } from './chart';

// Times are milliseconds; one step is 60 s.
const S = 60_000;
const base = { from: 0, to: 600 * 1000, step: S, width: 600, height: 200, pad: { l: 0, r: 0, t: 0, b: 0 } };
const ok = (steps: number, v: number): MetricPoint => ({ t: steps * S, v, state: 'ok' });
const none = (steps: number): MetricPoint => ({ t: steps * S, v: null, state: 'no_data' });
const gone = (steps: number): MetricPoint => ({ t: steps * S, v: null, state: 'unavailable' });
const series = (...points: MetricPoint[]): MetricSeries => ({ labels: {}, points });

describe('chart geometry', () => {
	it('splits runs at no_data steps and reports a typed gap', () => {
		const c = buildChart({ ...base, series: series(ok(0, 1), ok(1, 2), none(2), none(3), ok(4, 3), ok(5, 4), ok(6, 5), ok(7, 5), ok(8, 5), ok(9, 5), ok(10, 5)) });
		expect(c.runs.length).toBe(2);
		expect(c.gaps).toEqual([{ x1: 60, x2: 240, kind: 'no_data' }]);
	});
	it('keeps unavailable stronger than no_data in the same gap', () => {
		const c = buildChart({ ...base, series: series(ok(0, 1), none(1), gone(2), ok(3, 2), ok(4, 2), ok(5, 2), ok(6, 2), ok(7, 2), ok(8, 2), ok(9, 2), ok(10, 2)) });
		expect(c.gaps).toHaveLength(1);
		expect(c.gaps[0].kind).toBe('unavailable');
	});
	it('treats missing steps as a gap, including leading and trailing ones', () => {
		const c = buildChart({ ...base, series: series(ok(5, 1), ok(6, 2)) });
		expect(c.gaps).toHaveLength(2);
		expect(c.gaps.every((g) => g.kind === 'missing')).toBe(true);
		expect(c.runs).toHaveLength(1);
	});
	it('shows a trailing non-ok step as a gap even without a long hole', () => {
		const c = buildChart({ ...base, to: 300_000, series: series(ok(3, 1), ok(4, 2), gone(5)) });
		expect(c.gaps.at(-1)).toMatchObject({ kind: 'unavailable' });
	});
	it('with no real values everything is a gap and nothing is drawn', () => {
		expect(buildChart({ ...base, series: series(none(0), none(1)) }).gaps).toEqual([{ x1: 0, x2: 600, kind: 'no_data' }]);
		const u = buildChart({ ...base, series: series(none(0), gone(1)) });
		expect(u.hasData).toBe(false);
		expect(u.runs).toEqual([]);
		expect(u.gaps).toEqual([{ x1: 0, x2: 600, kind: 'unavailable' }]);
		expect(buildChart({ ...base, series: series() }).gaps[0].kind).toBe('missing');
	});
	it('never draws a value for a non-ok point even if one is present', () => {
		const rogue = { t: S, v: 99, state: 'unavailable' } as unknown as MetricPoint;
		const c = buildChart({ ...base, series: series(ok(0, 1), rogue, ok(2, 1)) });
		expect(c.valuePoints).toBe(2);
		expect(c.gaps.some((g) => g.kind === 'unavailable')).toBe(true);
	});
	it('draws reported unreadable intervals as unavailable, clipped to the range', () => {
		const c = buildChart({ ...base, gaps: [{ from: -1000, to: 120_000, reason: 'archive' }, { from: 900_000, to: 990_000, reason: 'x' }], series: series(ok(0, 1), ok(1, 1), ok(2, 1), ok(3, 1), ok(4, 1), ok(5, 1), ok(6, 1), ok(7, 1), ok(8, 1), ok(9, 1), ok(10, 1)) });
		expect(c.gaps).toEqual([{ x1: 0, x2: 120, kind: 'unavailable' }]);
	});
	it('handles a flat series and produces ticks', () => {
		const c = buildChart({ ...base, series: series(ok(0, 5), ok(1, 5)) });
		expect(c.yMax).toBeGreaterThan(c.yMin);
		expect(niceTicks(0, 97)).toEqual([0, 20, 40, 60, 80]);
	});
});

describe('currentValue', () => {
	const end = 10 * S;
	it('is a value only for a recent ok point', () => {
		expect(currentValue(series(ok(9, 4)), end, S)).toEqual({ kind: 'value', value: 4, time: 9 * S });
		expect(currentValue(series(ok(2, 4)), end, S)).toMatchObject({ kind: 'stale', value: 4 });
	});
	it('is unknown when the newest step is not ok', () => {
		expect(currentValue(series(ok(8, 4), gone(9)), end, S)).toEqual({ kind: 'unavailable' });
		expect(currentValue(series(ok(8, 4), none(9)), end, S)).toEqual({ kind: 'no_data' });
	});
	it('reports unavailable over no_data when there is no value at all', () => {
		expect(currentValue(series(none(1), gone(2)), end, S)).toEqual({ kind: 'unavailable' });
		expect(currentValue(series(none(1)), end, S)).toEqual({ kind: 'no_data' });
		expect(currentValue(series(), end, S)).toEqual({ kind: 'no_data' });
	});
});

describe('labelText', () => {
	it('drops the name label and sorts the rest', () => {
		expect(labelText({ name: 'cpu', slot: 'n1', host: 'h' })).toBe('host=h, slot=n1');
		expect(labelText({})).toBe('');
	});
});
