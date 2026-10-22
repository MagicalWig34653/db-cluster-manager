import { asApiError, type ApiError } from './api';
import { noteError } from './auth.svelte';

export type LoadState<T> = { status: 'loading' } | { status: 'error'; error: ApiError } | { status: 'ok'; value: T };

/** Runs a request and exposes its state; failures never produce placeholder data. */
export function load<T>(fn: () => Promise<T>) {
	let state = $state<LoadState<T>>({ status: 'loading' });
	let generation = 0;
	function run(keep = false) {
		const g = ++generation;
		// A refresh keeps the shown value until the answer arrives, so lists do not flicker or lose UI state.
		if (!(keep && state.status === 'ok')) state = { status: 'loading' };
		fn().then(
			(value) => {
				if (g === generation) state = { status: 'ok', value };
			},
			(e) => {
				const error = asApiError(e);
				noteError(error);
				if (g === generation) state = { status: 'error', error };
			}
		);
	}
	run();
	return {
		get state() {
			return state;
		},
		reload: () => run(),
		/** Re-requests without blanking the current value; errors still replace it. */
		refresh: () => run(true)
	};
}
