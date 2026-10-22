import { describe, expect, it } from 'vitest';
import { buildChart, latest, niceTicks } from './chart';

const base = { from: 0, to: 600, step: 60, width: 600, height: 200, pad: { l: 0, r: 0, t: 0, b: 0 } };

describe('chart geometry', () => {
	it('splits runs at null values and reports the gap', () => {
		const c = buildChart({
			...base,
			series: { points: [[0, 1], [60, 2], [120, null], [180, null], [240, 3], [300, 4], [360, 5], [420, 5], [480, 5], [540, 5], [600, 5]] }
		});
		expect(c.runs.length).toBe(2);
		expect(c.gaps.length).toBe(1);
		expect(c.gaps[0].x1).toBe(60);
		expect(c.gaps[0].x2).toBe(240);
	});
	it('treats missing steps as a gap, including leading and trailing ones', () => {
		const c = buildChart({ ...base, series: { points: [[300, 1], [360, 2]] } });
		expect(c.gaps).toHaveLength(2);
		expect(c.runs).toHaveLength(1);
	});
	it('with no real values everything is a gap and nothing is drawn', () => {
		const c = buildChart({ ...base, series: { points: [[0, null], [60, null]] } });
		expect(c.hasData).toBe(false);
		expect(c.runs).toEqual([]);
		expect(c.gaps).toEqual([{ x1: 0, x2: 600 }]);
	});
	it('handles a flat series and produces ticks', () => {
		const c = buildChart({ ...base, series: { points: [[0, 5], [60, 5]] } });
		expect(c.yMax).toBeGreaterThan(c.yMin);
		expect(niceTicks(0, 97)).toEqual([0, 20, 40, 60, 80]);
	});
	it('finds the latest real value', () => {
		expect(latest([{ points: [[1, 2], [5, null], [3, 9]] }])).toEqual({ value: 9, time: 3 });
		expect(latest([{ points: [[1, null]] }])).toBeUndefined();
	});
});
