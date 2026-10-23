<script lang="ts">
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { api } from '../../api';
	import { dateTime } from '../../format';
	import { load } from '../../load.svelte';
	import { driftLabel, driftOrdered, driftSummary } from '../../platform';
	import { locale, t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import { Button } from '../ui/button/index.js';
	import ApiGate from './ApiGate.svelte';
	import Loading from './Loading.svelte';

	// Read-only comparison of the cluster record with the Swarm services.
	let { cluster }: { cluster: string } = $props();
	// svelte-ignore state_referenced_locally
	const drift = load(() => api.clusterDrift(cluster));
</script>

<Card.Root class="lg:col-span-2">
	<Card.Header>
		<Card.Title><h2>{t('driftTitle')}</h2></Card.Title>
		<Card.Description>{t('driftIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="space-y-3 text-sm">
		{#if drift.state.status === 'loading'}
			<Loading />
		{:else if drift.state.status === 'error'}
			<ApiGate error={drift.state.error} retry={drift.reload} />
		{:else}
			{@const r = drift.state.value}
			{@const sum = driftSummary(r)}
			<div class="flex flex-wrap items-center gap-3">
				{#if sum.inSync}
					<p class="inline-flex items-center gap-1.5 font-medium text-emerald-700 dark:text-emerald-400"><CircleCheck class="size-4" aria-hidden="true" />{t('driftInSync')}</p>
				{:else}
					<p class="inline-flex items-center gap-1.5 font-medium text-amber-700 dark:text-amber-400"><TriangleAlert class="size-4" aria-hidden="true" />{t('driftFound', { count: sum.count })}</p>
				{/if}
				<span class="text-xs text-muted-foreground">{t('driftChecked', { at: dateTime(r.checkedAt, locale.value) ?? r.checkedAt })}</span>
				<Button size="sm" variant="outline" onclick={drift.refresh}>{t('driftRun')}</Button>
			</div>
			{#if r.findings.length > 0}
				<ul class="space-y-2">
					{#each driftOrdered(r.findings) as f, i (i)}
						<li class="rounded-md border p-3">
							<p class="font-medium">{t(driftLabel[f.kind])}{#if f.slot} <span class="font-mono text-xs text-muted-foreground">({f.slot})</span>{/if}{#if f.service} <span class="font-mono text-xs text-muted-foreground">{f.service}</span>{/if}</p>
							<p class="mt-1">{f.detail}</p>
							<p class="mt-1 text-muted-foreground"><span class="font-medium">{t('driftAction')}:</span> {f.action}</p>
						</li>
					{/each}
				</ul>
			{/if}
		{/if}
	</Card.Content>
</Card.Root>
