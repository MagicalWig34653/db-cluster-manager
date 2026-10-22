<script lang="ts">
	import { api } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const clusters = load(api.clusters);
</script>

<PageHeader title={t('clustersTitle')} intro={t('clustersIntro')} />

{#if clusters.state.status === 'loading'}
	<Loading />
{:else if clusters.state.status === 'error'}
	<ErrorNotice error={clusters.state.error} retry={clusters.reload} />
{:else if clusters.state.value.length === 0}
	<p class="text-sm text-muted-foreground">{t('clustersEmpty')}</p>
{:else}
	<div class="rounded-lg border">
		<Table.Root>
			<Table.Header>
				<Table.Row>
					<Table.Head>{t('colName')}</Table.Head>
					<Table.Head>{t('colSwarm')}</Table.Head>
					<Table.Head>{t('colEngine')}</Table.Head>
					<Table.Head>{t('colState')}</Table.Head>
					<Table.Head>{t('colNodes')}</Table.Head>
					<Table.Head>{t('colFreshness')}</Table.Head>
				</Table.Row>
			</Table.Header>
			<Table.Body>
				{#each clusters.state.value as c (c.id)}
					<Table.Row>
						<Table.Cell class="font-medium">
							<a class="underline-offset-4 hover:underline focus-visible:underline" href="/clusters/{encodeURIComponent(c.id)}">{c.name}</a>
						</Table.Cell>
						<Table.Cell class="font-mono text-xs">{c.swarm}</Table.Cell>
						<Table.Cell>{c.engine ?? t('stateUnknown')}</Table.Cell>
						<Table.Cell><Status value={c.state} /></Table.Cell>
						<Table.Cell>{t('nodeCount', { count: c.nodes.length })}</Table.Cell>
						<Table.Cell><Freshness at={c.observedAt} /></Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
{/if}
