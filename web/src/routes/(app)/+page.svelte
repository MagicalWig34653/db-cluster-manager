<script lang="ts">
	import { api, type State } from '../../lib/api';
	import { auth } from '../../lib/auth.svelte';
	import { load } from '../../lib/load.svelte';
	import { t } from '../../lib/locale.svelte';
	import * as Card from '../../lib/components/ui/card/index.js';
	import PageHeader from '../../lib/components/app/PageHeader.svelte';
	import Status from '../../lib/components/app/Status.svelte';
	import ErrorNotice from '../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../lib/components/app/Loading.svelte';

	const health = load(api.health);
	const info = load(api.version);
	const signedIn = auth.state.status === 'signedIn';
	const swarms = signedIn ? load(api.swarms) : null;
	const clusters = signedIn ? load(api.clusters) : null;

	const order: State[] = ['healthy', 'degraded', 'unavailable', 'unknown'];
	function countByState(items: { state: State }[]) {
		return order.map((s) => ({ state: s, count: items.filter((i) => i.state === s).length })).filter((c) => c.count > 0);
	}
</script>

<PageHeader title={t('overviewTitle')} intro={t('overviewIntro')} />

<div class="grid gap-4 md:grid-cols-2">
	<Card.Root>
		<Card.Header>
			<Card.Title><h2>{t('processStatus')}</h2></Card.Title>
			<Card.Description>{t('healthOnlyProcess')}</Card.Description>
		</Card.Header>
		<Card.Content>
			<dl class="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
				<dt class="text-muted-foreground">{t('apiHealth')}</dt>
				<dd>
					{#if health.state.status === 'loading'}<Loading />{:else if health.state.status === 'error'}<Status
							value="unavailable"
						/>{:else if health.state.value.status === 'ok'}{t('healthOk')}{:else}<Status value="unknown" />{/if}
				</dd>
				{#if info.state.status === 'ok'}
					<dt class="text-muted-foreground">{t('version')}</dt><dd class="font-mono">{info.state.value.version}</dd>
					<dt class="text-muted-foreground">{t('commit')}</dt><dd class="font-mono">{info.state.value.commit}</dd>
					<dt class="text-muted-foreground">{t('go')}</dt><dd class="font-mono">{info.state.value.goVersion}</dd>
					<dt class="text-muted-foreground">{t('platform')}</dt><dd class="font-mono">{info.state.value.platform}</dd>
				{/if}
			</dl>
			{#if info.state.status === 'error'}<div class="mt-3"><ErrorNotice error={info.state.error} retry={info.reload} /></div>{/if}
		</Card.Content>
	</Card.Root>

	{#if swarms && clusters}
		<Card.Root>
			<Card.Header>
				<Card.Title><h2>{t('inventory')}</h2></Card.Title>
			</Card.Header>
			<Card.Content class="space-y-4">
				{#each [{ label: t('countSwarms'), href: '/swarms', l: swarms }, { label: t('countClusters'), href: '/clusters', l: clusters }] as row (row.href)}
					<div>
						<h3 class="mb-1 text-sm font-medium"><a class="underline-offset-4 hover:underline" href={row.href}>{row.label}</a></h3>
						{#if row.l.state.status === 'loading'}
							<Loading />
						{:else if row.l.state.status === 'error'}
							<ErrorNotice error={row.l.state.error} retry={row.l.reload} />
						{:else if row.l.state.value.length === 0}
							<p class="text-sm text-muted-foreground">0</p>
						{:else}
							<p class="text-sm"><span class="text-2xl font-semibold">{row.l.state.value.length}</span></p>
							<ul class="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-sm" aria-label={t('byState')}>
								{#each countByState(row.l.state.value) as c (c.state)}
									<li class="flex items-center gap-1"><Status value={c.state} />: {c.count}</li>
								{/each}
							</ul>
						{/if}
					</div>
				{/each}
			</Card.Content>
		</Card.Root>
	{/if}
</div>
