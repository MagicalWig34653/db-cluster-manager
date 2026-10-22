<script lang="ts">
	import CircleDashed from '@lucide/svelte/icons/circle-dashed';
	import type { ApiError } from '../../api';
	import { t } from '../../locale.svelte';
	import ErrorNotice from './ErrorNotice.svelte';

	let { error, retry }: { error: ApiError; retry?: () => void } = $props();
	// 404/503 on an endpoint this UI knows only from its assumed contract means
	// the manager does not offer it (yet); nothing is invented in its place.
	// A coarse code (for example not_leader) is a real answer, not a missing endpoint.
	const missing = $derived(!error.code && (error.kind === 'not_found' || error.kind === 'unavailable'));
</script>

{#if missing}
	<div class="rounded-lg border border-dashed p-4 text-sm" role="status">
		<p class="flex items-center gap-2 font-medium"><CircleDashed class="size-4" aria-hidden="true" />{t('apiNotAvailable')}</p>
		<p class="mt-1 text-muted-foreground">{t('apiNotAvailableBody')}</p>
	</div>
{:else}
	<ErrorNotice {error} {retry} />
{/if}
