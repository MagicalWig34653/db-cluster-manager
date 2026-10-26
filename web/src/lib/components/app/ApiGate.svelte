<script lang="ts">
	import type { ApiError } from '../../api';
	import ErrorNotice from './ErrorNotice.svelte';
	import StateView from './StateView.svelte';

	let { error, retry }: { error: ApiError; retry?: () => void } = $props();
	// 404/503 on an endpoint this UI knows only from its assumed contract means
	// the manager does not offer it (yet); nothing is invented in its place.
	// A coarse code (for example not_leader) is a real answer, not a missing endpoint.
	const missing = $derived(!error.code && (error.kind === 'not_found' || error.kind === 'unavailable'));
</script>

{#if missing}
	<StateView kind="empty" title="apiNotAvailable" message="apiNotAvailableBody" />
{:else}
	<ErrorNotice {error} {retry} />
{/if}
