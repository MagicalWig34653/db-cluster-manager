<script lang="ts">
	import StateView from '../../../lib/components/app/StateView.svelte';

	import { api } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	// Database instances are the engines behind the clusters; logical databases
	// and users are not reported by the API, so they are not listed.
	const clusters = load(api.clusters);
</script>

<PageHeader title={t('navDatabases')} intro={t('databasesIntro')} />

{#if clusters.state.status === 'loading'}
	<Loading />
{:else if clusters.state.status === 'error'}
	<ErrorNotice error={clusters.state.error} retry={clusters.reload} />
{:else if clusters.state.value.length === 0}
	<StateView kind="empty" message="clustersEmpty" />
{:else}
	<div class="rounded-lg border">
		<Table.Root>
			<Table.Header>
				<Table.Row>
					<Table.Head>{t('colName')}</Table.Head>
					<Table.Head>{t('colEngine')}</Table.Head>
					<Table.Head>{t('endpoint')}</Table.Head>
					<Table.Head>{t('colState')}</Table.Head>
					<Table.Head>{t('colNodes')}</Table.Head>
				</Table.Row>
			</Table.Header>
			<Table.Body>
				{#each clusters.state.value as c (c.id)}
					<Table.Row>
						<Table.Cell class="font-medium">
							<a class="underline-offset-4 hover:underline focus-visible:underline" href="/clusters/{encodeURIComponent(c.id)}">{c.name}</a>
						</Table.Cell>
						<Table.Cell>{c.engine ?? t('stateUnknown')}</Table.Cell>
						<Table.Cell class="font-mono text-xs">
							{#if c.endpoints && c.endpoints.length > 0}{c.endpoints.map((e) => `${e.host}:${e.port}`).join(', ')}{:else}{t('notReported')}{/if}
						</Table.Cell>
						<Table.Cell><Status value={c.state} /></Table.Cell>
						<Table.Cell>{t('nodeCount', { count: c.nodes.length })}</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
	<p class="mt-3 text-xs text-muted-foreground">{t('databasesHint')}</p>
{/if}
