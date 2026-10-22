import { describe, expect, it } from 'vitest';
import { canEditDashboard, defaultPanel, sortPanels, spanOf, validateDashboard } from './dashboard';

describe('dashboard helpers', () => {
	const good = { ...defaultPanel(0, 'p'), title: 'T', selector: 'cpu{host="a"}' };
	it('places new panels two per row on the 24-column grid', () => {
		expect(defaultPanel(0, 'a')).toMatchObject({ x: 0, y: 0, w: 12, h: 6 });
		expect(defaultPanel(1, 'b')).toMatchObject({ x: 12, y: 0 });
		expect(defaultPanel(2, 'c')).toMatchObject({ x: 0, y: 6 });
	});
	it('accepts a valid dashboard and flags what the manager would refuse', () => {
		expect(validateDashboard({ title: 'D', panels: [good] })).toEqual([]);
		const bad = { ...good, title: '', selector: ' ', func: 'median', stepSeconds: 0, windowSeconds: -1 };
		const issues = validateDashboard({ title: ' ', panels: [bad] }).map((i) => i.field);
		expect(issues).toEqual(['title', 'panelTitle', 'selector', 'func', 'step', 'window']);
		expect(validateDashboard({ title: 'D', panels: [{ ...good, func: 'quantile', quantile: 1.5 }] })[0].field).toBe('quantile');
		expect(validateDashboard({ title: 'D', panels: [{ ...good, func: 'quantile', quantile: 0.9 }] })).toEqual([]);
	});
	it('orders panels by row then column and clamps spans', () => {
		const a = { ...good, id: 'a', x: 12, y: 0 };
		const b = { ...good, id: 'b', x: 0, y: 6 };
		const c = { ...good, id: 'c', x: 0, y: 0 };
		expect(sortPanels([a, b, c]).map((p) => p.id)).toEqual(['c', 'a', 'b']);
		expect(spanOf({ ...good, w: 99 })).toBe(24);
		expect(spanOf({ ...good, w: 0 })).toBe(1);
	});
	it('lets only the owner edit and never a template', () => {
		expect(canEditDashboard({ owner: 'me' }, 'me')).toBe(true);
		expect(canEditDashboard({ owner: 'other' }, 'me')).toBe(false);
		expect(canEditDashboard({ owner: 'me', template: true }, 'me')).toBe(false);
		expect(canEditDashboard({ owner: 'me' }, undefined)).toBe(false);
	});
});
