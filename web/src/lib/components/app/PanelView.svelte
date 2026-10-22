<script lang="ts">
	import { api, type MetricsResult, type Panel } from '../../api';
	import { latest } from '../../chart';
	import { load } from '../../load.svelte';
	import { locale, t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import ApiGate from './ApiGate.svelte';
	import LineChart from './LineChart.svelte';
	import Loading from './Loading.svelte';
	import Status from './Status.svelte';

	let { panel, rangeSeconds = 3600 }: { panel: Panel; rangeSeconds?: number } = $props();
	const step = 60;
	const to = Math.floor(Date.now() / 1000);
	// svelte-ignore state_referenced_locally
	const from = to - rangeSeconds;
	// Assumed: POST /api/v1/metrics/query { query, from, to, step } -> { series: [{ points: [[t, v|null]] }] }.
	// svelte-ignore state_referenced_locally
	const result = load<MetricsResult>(() =>
		api.queryMetrics({ query: panel.query, from: new Date(from * 1000).toISOString(), to: new Date(to * 1000).toISOString(), step })
	);
	const fmt = (v: number) => new Intl.NumberFormat(locale.value, { maximumFractionDigits: 2 }).format(v);
</script>

<Card.Root>
	<Card.Header>
		<Card.Title><h3>{panel.title}</h3></Card.Title>
		<Card.Description class="font-mono text-xs">{panel.query}</Card.Description>
	</Card.Header>
	<Card.Content>
		{#if result.state.status === 'loading'}
			<Loading />
		{:else if result.state.status === 'error'}
			<ApiGate error={result.state.error} retry={result.reload} />
		{:else if panel.kind === 'line'}
			{#if result.state.value.series.length === 0}
				<p class="text-sm text-muted-foreground">{t('chartNoData', { label: panel.title })}</p>
			{:else}
				{#each result.state.value.series as s, i (i)}
					<LineChart series={s} {from} {to} {step} label={panel.title} unit={panel.unit ?? ''} />
				{/each}
			{/if}
		{:else}
			{@const l = latest(result.state.value.series)}
			<!-- A stat is only shown as current when its newest real point is recent; otherwise it is unknown. -->
			{#if l && to - l.time <= step * 3}
				<p class="text-4xl font-semibold tabular-nums">{fmt(l.value)}<span class="ml-1 text-lg text-muted-foreground">{panel.unit ?? ''}</span></p>
			{:else if l}
				<p class="text-sm"><Status value="unknown" /> — {t('statStale', { value: fmt(l.value), age: Math.round((to - l.time) / 60) })}</p>
			{:else}
				<p class="text-sm"><Status value="unknown" /> — {t('chartNoData', { label: panel.title })}</p>
			{/if}
		{/if}
	</Card.Content>
</Card.Root>
