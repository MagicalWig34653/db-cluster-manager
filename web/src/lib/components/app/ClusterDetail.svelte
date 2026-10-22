<script lang="ts">
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import { api } from '../../api';
	import { load } from '../../load.svelte';
	import { t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import * as Table from '../ui/table/index.js';
	import PageHeader from './PageHeader.svelte';
	import Status from './Status.svelte';
	import Freshness from './Freshness.svelte';
	import ErrorNotice from './ErrorNotice.svelte';
	import Loading from './Loading.svelte';

	let { id }: { id: string } = $props();
	// The parent re-creates this component when the id changes.
	// svelte-ignore state_referenced_locally
	const cluster = load(() => api.cluster(id));
</script>

{#snippet back()}
	<a href="/clusters" class="inline-flex items-center gap-1 text-sm text-muted-foreground underline-offset-4 hover:underline">
		<ArrowLeft class="size-4" aria-hidden="true" />{t('backToClusters')}
	</a>
{/snippet}

<!-- Values the API contract does not carry yet are rendered as unknown, never as healthy. -->
{#snippet notReported()}
	<span class="inline-flex flex-wrap items-center gap-x-2">
		<Status value="unknown" />
		<span class="text-xs text-muted-foreground">({t('notReported')})</span>
	</span>
{/snippet}

{#if cluster.state.status === 'loading'}
	<PageHeader title={t('clusterDetail')} />
	<Loading />
{:else if cluster.state.status === 'error'}
	<PageHeader title={t('clusterDetail')} />
	<div class="space-y-4">
		<ErrorNotice error={cluster.state.error} retry={cluster.reload} />
		{@render back()}
	</div>
{:else}
	{@const c = cluster.state.value}
	<div class="mb-3">{@render back()}</div>
	<PageHeader title={c.name}>
		{#snippet actions()}<Status value={c.state} class="text-base" />{/snippet}
	</PageHeader>

	<div class="grid gap-4 lg:grid-cols-[1fr_1fr]">
		<Card.Root>
			<Card.Content>
				<dl class="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-3 text-sm">
					<dt class="text-muted-foreground">{t('colState')}</dt>
					<dd class="flex flex-wrap items-center gap-x-3"><Status value={c.state} /><Freshness at={c.observedAt} /></dd>
					<dt class="text-muted-foreground">{t('colId')}</dt>
					<dd class="font-mono text-xs">{c.id}</dd>
					<dt class="text-muted-foreground">{t('colSwarm')}</dt>
					<dd class="font-mono text-xs">{c.swarm}</dd>
					<dt class="text-muted-foreground">{t('colEngine')}</dt>
					<dd>{#if c.engine}{c.engine}{:else}{@render notReported()}{/if}</dd>
					<dt class="text-muted-foreground">{t('endpoint')}</dt>
					<dd>{@render notReported()}</dd>
					<dt class="text-muted-foreground">{t('lastVerifiedRestore')}</dt>
					<dd>{@render notReported()}</dd>
					<dt class="text-muted-foreground">{t('automationPolicy')}</dt>
					<dd>{@render notReported()}</dd>
				</dl>
				<p class="mt-4 text-xs text-muted-foreground">{t('notReportedHint')}</p>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header><Card.Title><h2>{t('nodes')}</h2></Card.Title></Card.Header>
			<Card.Content>
				{#if c.nodes.length === 0}
					<p class="text-sm text-muted-foreground">{t('noNodes')}</p>
				{:else}
					<Table.Root>
						<Table.Header>
							<Table.Row>
								<Table.Head>{t('colId')}</Table.Head>
								<Table.Head>{t('colHost')}</Table.Head>
								<Table.Head>{t('colRole')}</Table.Head>
								<Table.Head>{t('colState')}</Table.Head>
							</Table.Row>
						</Table.Header>
						<Table.Body>
							{#each c.nodes as n (n.id)}
								<Table.Row>
									<Table.Cell class="font-mono text-xs">{n.id}</Table.Cell>
									<Table.Cell>{#if n.host}{n.host}{:else}<Status value="unknown" />{/if}</Table.Cell>
									<Table.Cell><Status value="unknown" /></Table.Cell>
									<Table.Cell><Status value={n.state} /></Table.Cell>
								</Table.Row>
							{/each}
						</Table.Body>
					</Table.Root>
				{/if}
			</Card.Content>
		</Card.Root>
	</div>
{/if}
