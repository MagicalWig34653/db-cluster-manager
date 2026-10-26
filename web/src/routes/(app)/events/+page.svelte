<script lang="ts">
	import StateView from '../../../lib/components/app/StateView.svelte';

	import { api } from '../../../lib/api';
	import { dateTime, newestFirst, relativeAge } from '../../../lib/format';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const events = load(api.events);
	const sorted = $derived(events.state.status === 'ok' ? newestFirst(events.state.value, (e) => e.time) : []);
</script>

<PageHeader title={t('eventsTitle')} intro={t('eventsIntro')} />

{#if events.state.status === 'loading'}
	<Loading />
{:else if events.state.status === 'error'}
	<ErrorNotice error={events.state.error} retry={events.reload} />
{:else if sorted.length === 0}
	<StateView kind="empty" message="eventsEmpty" />
{:else}
	<div class="rounded-lg border">
		<Table.Root>
			<Table.Header>
				<Table.Row>
					<Table.Head>{t('colTime')}</Table.Head>
					<Table.Head>{t('colState')}</Table.Head>
					<Table.Head>{t('colTarget')}</Table.Head>
					<Table.Head>{t('colMessage')}</Table.Head>
				</Table.Row>
			</Table.Header>
			<Table.Body>
				{#each sorted as e (e.id)}
					<Table.Row>
						<Table.Cell class="whitespace-nowrap">
							<time datetime={e.time} title={dateTime(e.time, locale.value)}>{relativeAge(e.time, locale.value)}</time>
						</Table.Cell>
						<Table.Cell><Status kind="severity" value={e.severity} /></Table.Cell>
						<Table.Cell class="text-xs">
							{#if e.cluster}<a class="underline-offset-4 hover:underline" href="/clusters/{encodeURIComponent(e.cluster)}">{e.cluster}</a>{/if}
							{#if e.swarm}<span class="font-mono text-muted-foreground">{e.swarm}</span>{/if}
							{#if !e.cluster && !e.swarm}<span class="text-muted-foreground">{t('managerLevel')}</span>{/if}
						</Table.Cell>
						<Table.Cell class="whitespace-normal">{e.message}</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
{/if}
