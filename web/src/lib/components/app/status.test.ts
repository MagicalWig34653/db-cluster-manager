import { describe, expect, it } from 'vitest';
import { statusView } from './status';

describe('statusView', () => {
	it('maps known values', () => {
		expect(statusView('state', 'healthy')).toMatchObject({ label: 'stateHealthy', tone: 'ok' });
		expect(statusView('operation', 'failed')).toMatchObject({ label: 'opFailed', tone: 'bad' });
		expect(statusView('severity', 'warning')).toMatchObject({ label: 'sevWarning', tone: 'warn' });
	});
	it('renders anything unrecognized as unknown, never healthy', () => {
		for (const v of ['', 'ok', 'toString', '__proto__', 'HEALTHY'])
			for (const k of ['state', 'operation', 'severity'] as const)
				expect(statusView(k, v)).toMatchObject({ label: 'stateUnknown', tone: 'unknown' });
	});
});
