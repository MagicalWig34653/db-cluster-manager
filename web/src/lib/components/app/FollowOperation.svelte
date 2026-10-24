<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '../../api';
	import { load } from '../../load.svelte';
	import { t } from '../../locale.svelte';
	import ApiGate from './ApiGate.svelte';
	import OperationCard from './OperationCard.svelte';

	// Shows one operation (plan, progress, approval) taken from the operations
	// list and keeps it fresh while it is not finished.
	let { id }: { id: string } = $props();
	const ops = load(api.operations);
	const op = $derived(ops.state.status === 'ok' ? ops.state.value.find((o) => o.id === id) : undefined);
	const finished = $derived(op?.state === 'succeeded' || op?.state === 'failed' || op?.state === 'aborted');
	onMount(() => {
		const timer = setInterval(() => {
			if (!finished) ops.refresh();
		}, 5000);
		return () => clearInterval(timer);
	});
</script>

<div class="rounded-md border p-3" aria-live="polite">
	{#if ops.state.status === 'error'}
		<ApiGate error={ops.state.error} retry={ops.reload} />
	{:else if op}
		<OperationCard {op} onchange={ops.refresh} />
	{:else if ops.state.status === 'loading'}
		<p class="text-sm text-muted-foreground">{t('followLoading')}</p>
	{:else}
		<p class="text-sm text-muted-foreground">{t('followMissing')}</p>
	{/if}
	<p class="mt-2 text-xs"><a class="underline underline-offset-4" href="/operations#{encodeURIComponent(id)}">{t('openInOperations')}</a></p>
</div>
