import { describe, expect, it } from 'vitest';
import { ApiError } from './api';
import { errorMessage, newestFirst, recoveryLinkParams, relativeAge } from './format';

describe('format', () => {
	const now = Date.parse('2026-10-20T12:00:00Z');
	it('formats relative ages and treats missing or invalid times as unknown', () => {
		expect(relativeAge('2026-10-20T11:57:00Z', 'en', now)).toBe('3 minutes ago');
		expect(relativeAge('2026-10-20T10:00:00Z', 'de', now)).toBe('vor 2 Stunden');
		expect(relativeAge(undefined, 'en', now)).toBeUndefined();
		expect(relativeAge('nope', 'en', now)).toBeUndefined();
	});
	it('sorts newest first and keeps undated entries last', () => {
		const items = [{ t: undefined, n: 'a' }, { t: '2026-01-01T00:00:00Z', n: 'b' }, { t: '2026-02-01T00:00:00Z', n: 'c' }, { t: undefined, n: 'd' }];
		expect(newestFirst(items, (i) => i.t).map((i) => i.n)).toEqual(['c', 'b', 'a', 'd']);
	});
	it('maps errors to localized keys only', () => {
		expect(errorMessage(new ApiError('unavailable', 503)).key).toBe('errUnavailable');
		expect(errorMessage(new ApiError('rate_limited', 429, 12))).toEqual({ key: 'errRateLimitedSeconds', params: { seconds: 12 } });
		expect(errorMessage(new ApiError('server_error', 500)).key).toBe('errServer');
	});
	it('reads recovery link parameters from fragment first, then query', () => {
		expect(recoveryLinkParams(new URL('https://m.example/recover/link#account=admin&token=abc'))).toEqual({ account: 'admin', token: 'abc' });
		expect(recoveryLinkParams(new URL('https://m.example/recover/link?account=x&token=y#token=z'))).toEqual({ account: 'x', token: 'z' });
		expect(recoveryLinkParams(new URL('https://m.example/recover/link'))).toEqual({ account: undefined, token: undefined });
	});
});
