// Authentication state of this browser, derived only from the manager API.

import { api, asApiError, type ApiError, type SessionInfo } from './api';

export type AuthState =
	| { status: 'loading' }
	/** The manager runs without an auth configuration: only /health and /version exist. */
	| { status: 'disabled' }
	| { status: 'anonymous'; setupRequired: boolean }
	| { status: 'signedIn'; session: SessionInfo }
	| { status: 'error'; error: ApiError };

let state = $state<AuthState>({ status: 'loading' });

async function refresh() {
	let setupRequired: boolean;
	try {
		setupRequired = (await api.setupStatus()).setupRequired;
	} catch (e) {
		const err = asApiError(e);
		state = err.kind === 'not_found' ? { status: 'disabled' } : { status: 'error', error: err };
		return;
	}
	try {
		state = { status: 'signedIn', session: await api.session() };
	} catch (e) {
		const err = asApiError(e);
		state = err.kind === 'unauthenticated' ? { status: 'anonymous', setupRequired } : { status: 'error', error: err };
	}
}

export const auth = {
	get state() {
		return state;
	},
	refresh,
	/** Called when any request reports that the session is gone. */
	expired() {
		state = { status: 'anonymous', setupRequired: false };
	},
	async logout() {
		try {
			await api.logout();
		} catch {
			// a 401 means the session was already gone; either way it is over here
		}
		state = { status: 'anonymous', setupRequired: false };
	}
};

/** Reacts to auth-relevant errors from data requests. */
export function noteError(e: ApiError) {
	if (e.kind === 'unauthenticated') auth.expired();
	else if (e.kind === 'passkey_enrollment_required') void refresh();
}
