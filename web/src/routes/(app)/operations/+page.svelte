<script lang="ts">
	import { api } from '../../../lib/api';
	import { newestFirst } from '../../../lib/format';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import OperationCard from '../../../lib/components/app/OperationCard.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const ops = load(api.operations);
	const sorted = $derived(ops.state.status === 'ok' ? newestFirst(ops.state.value, (o) => o.startedAt) : []);
</script>

<PageHeader title={t('operationsTitle')} intro={t('operationsIntro')} />

{#if ops.state.status === 'loading'}
	<Loading />
{:else if ops.state.status === 'error'}
	<ErrorNotice error={ops.state.error} retry={ops.reload} />
{:else if sorted.length === 0}
	<p class="text-sm text-muted-foreground">{t('operationsEmpty')}</p>
{:else}
	<p class="mb-4 text-xs text-muted-foreground">{t('opNoPause')}</p>
	<!-- Timeline, newest first. -->
	<ol class="relative ml-2 border-l pl-6">
		{#each sorted as o (o.id)}
			<li id={o.id} class="relative scroll-mt-4 pb-6 last:pb-0">
				<span class="absolute top-1.5 -left-[1.95rem] size-3 rounded-full border-2 border-background bg-border" aria-hidden="true"></span>
				<OperationCard op={o} onchange={ops.refresh} />
			</li>
		{/each}
	</ol>
{/if}
