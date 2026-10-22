import { ApiError } from './api';
import type { MessageKey } from './i18n';
import { CeremonyError, type CeremonyFailure } from './webauthn';

/** Relative age such as "3 minutes ago"; undefined input or invalid dates yield undefined. */
export function relativeAge(iso: string | undefined, locale: string, now = Date.now()): string | undefined {
	if (!iso) return undefined;
	const t = Date.parse(iso);
	if (Number.isNaN(t)) return undefined;
	const seconds = Math.round((t - now) / 1000);
	const rtf = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });
	const abs = Math.abs(seconds);
	if (abs < 60) return rtf.format(seconds, 'second');
	if (abs < 3600) return rtf.format(Math.round(seconds / 60), 'minute');
	if (abs < 86400) return rtf.format(Math.round(seconds / 3600), 'hour');
	return rtf.format(Math.round(seconds / 86400), 'day');
}

export function dateTime(iso: string | undefined, locale: string): string | undefined {
	if (!iso) return undefined;
	const t = Date.parse(iso);
	if (Number.isNaN(t)) return undefined;
	return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'medium' }).format(t);
}

/** Sorts newest first; entries without a time go last in their original order. */
export function newestFirst<T>(items: readonly T[], time: (item: T) => string | undefined): T[] {
	const key = (i: T) => {
		const v = time(i);
		const t = v ? Date.parse(v) : NaN;
		return Number.isNaN(t) ? -Infinity : t;
	};
	return items
		.map((item, index) => ({ item, index, k: key(item) }))
		.sort((a, b) => (b.k === a.k ? a.index - b.index : b.k - a.k))
		.map((e) => e.item);
}

/** Message for an API error; uses only the coarse classification, never server text. */
export function errorMessage(e: ApiError): { key: MessageKey; params?: Record<string, number> } {
	switch (e.kind) {
		case 'unauthenticated':
			return { key: 'errUnauthenticated' };
		case 'forbidden':
			return { key: 'errForbidden' };
		case 'origin_rejected':
			return { key: 'errOrigin' };
		case 'passkey_enrollment_required':
			return { key: 'errEnrollmentRequired' };
		case 'not_found':
			return { key: 'errNotFound' };
		case 'bad_request':
			return { key: 'errBadRequest' };
		case 'conflict':
			return { key: 'errConflict' };
		case 'rate_limited':
			return e.retryAfter ? { key: 'errRateLimitedSeconds', params: { seconds: e.retryAfter } } : { key: 'errRateLimited' };
		case 'unavailable':
			return { key: 'errUnavailable' };
		case 'invalid_response':
			return { key: 'errInvalidResponse' };
		case 'network':
			return { key: 'errNetwork' };
		default:
			return { key: 'errServer' };
	}
}

export function ceremonyMessage(reason: CeremonyFailure): MessageKey {
	return (
		{
			unsupported: 'ceremonyUnsupported',
			insecure: 'ceremonyInsecure',
			cancelled: 'ceremonyCancelled',
			exists: 'ceremonyExists',
			failed: 'ceremonyFailed'
		} as const
	)[reason];
}

/** Reads key=value pairs from a URL fragment (preferred: never sent to the server) or query. */
export function recoveryLinkParams(url: URL): { account?: string; token?: string } {
	const fromHash = new URLSearchParams(url.hash.replace(/^#/, ''));
	const pick = (name: string) => fromHash.get(name) ?? url.searchParams.get(name) ?? undefined;
	return { account: pick('account') || undefined, token: pick('token') || undefined };
}

export type Failure = { key: MessageKey; params?: Record<string, number> };

/** Localized failure for a passkey flow: browser ceremony errors or API errors. */
export function failureOf(e: unknown): Failure {
	if (e instanceof CeremonyError) return { key: ceremonyMessage(e.reason) };
	if (e instanceof ApiError) return errorMessage(e);
	return { key: 'ceremonyFailed' };
}
