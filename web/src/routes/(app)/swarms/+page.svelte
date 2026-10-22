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

	const swarms = load(api.swarms);
</script>

<PageHeader title={t('swarmsTitle')} intro={t('swarmsIntro')} />

{#if swarms.state.status === 'loading'}
	<Loading />
{:else if swarms.state.status === 'error'}
	<ErrorNotice error={swarms.state.error} retry={swarms.reload} />
{:else if swarms.state.value.length === 0}
	<p class="text-sm text-muted-foreground">{t('swarmsEmpty')}</p>
{:else}
	<div class="rounded-lg border">
		<Table.Root>
			<Table.Header>
				<Table.Row>
					<Table.Head>{t('colName')}</Table.Head>
					<Table.Head>{t('colId')}</Table.Head>
					<Table.Head>{t('colState')}</Table.Head>
					<Table.Head>{t('colFreshness')}</Table.Head>
				</Table.Row>
			</Table.Header>
			<Table.Body>
				{#each swarms.state.value as s (s.id)}
					<Table.Row>
						<Table.Cell class="font-medium">{s.name}</Table.Cell>
						<Table.Cell class="font-mono text-xs">{s.id}</Table.Cell>
						<Table.Cell><Status value={s.state} /></Table.Cell>
						<Table.Cell><Freshness at={s.observedAt} /></Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
{/if}
