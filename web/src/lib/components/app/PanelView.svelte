<script lang="ts">
	import { api, type MetricSeries, type MetricsResult, type Panel } from '../../api';
	import { currentValue, labelText } from '../../chart';
	import { load } from '../../load.svelte';
	import { locale, t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import * as Table from '../ui/table/index.js';
	import ApiGate from './ApiGate.svelte';
	import LineChart from './LineChart.svelte';
	import Loading from './Loading.svelte';
	import Status from './Status.svelte';

	let { panel, rangeSeconds = 3600 }: { panel: Panel; rangeSeconds?: number } = $props();
	// svelte-ignore state_referenced_locally
	const stepMs = panel.stepSeconds * 1000;
	const to = Date.now();
	// svelte-ignore state_referenced_locally
	const from = to - rangeSeconds * 1000;
	// POST /api/v1/metrics/query; times are milliseconds, points carry an explicit state.
	// svelte-ignore state_referenced_locally
	const result = load<MetricsResult>(() =>
		api.queryMetrics({
			selector: panel.selector,
			start: from,
			end: to,
			stepSeconds: panel.stepSeconds,
			windowSeconds: panel.windowSeconds,
			func: panel.func,
			...(panel.func === 'quantile' ? { quantile: panel.quantile ?? 0 } : {})
		})
	);
	const fmt = (v: number) => new Intl.NumberFormat(locale.value, { maximumFractionDigits: 2 }).format(v);
	const name = (s: MetricSeries) => labelText(s.labels) || s.labels.name || '';
</script>

{#snippet current(s: MetricSeries, big: boolean)}
	<!-- A value is shown as current only when its newest step is a recent "ok" point; everything else is unknown. -->
	{@const v = currentValue(s, to, stepMs)}
	{#if v.kind === 'value'}
		<span class={big ? 'text-4xl font-semibold tabular-nums' : 'tabular-nums'}>{fmt(v.value)}</span>
	{:else if v.kind === 'stale'}
		<span class="text-sm"><Status value="unknown" /> — {t('statStale', { value: fmt(v.value), age: Math.round(v.ageMs / 60000) })}</span>
	{:else if v.kind === 'unavailable'}
		<span class="text-sm"><Status value="unknown" /> — {t('statUnavailable')}</span>
	{:else}
		<span class="text-sm"><Status value="unknown" /> — {t('chartNoData', { label: panel.title })}</span>
	{/if}
{/snippet}

<Card.Root>
	<Card.Header>
		<Card.Title><h3>{panel.title}</h3></Card.Title>
		<Card.Description class="font-mono text-xs">{panel.func}({panel.selector})</Card.Description>
	</Card.Header>
	<Card.Content>
		{#if result.state.status === 'loading'}
			<Loading />
		{:else if result.state.status === 'error'}
			<ApiGate error={result.state.error} retry={result.reload} />
		{:else if result.state.value.series.length === 0}
			<p class="text-sm text-muted-foreground">{t('chartNoData', { label: panel.title })}</p>
		{:else if panel.kind === 'line'}
			{#each result.state.value.series as s, i (i)}
				{#if name(s)}<p class="mt-2 font-mono text-xs text-muted-foreground">{name(s)}</p>{/if}
				<LineChart series={s} gaps={result.state.value.gaps} {from} {to} step={stepMs} label={panel.title} />
			{/each}
		{:else if panel.kind === 'stat'}
			<div class="space-y-3">
				{#each result.state.value.series as s, i (i)}
					<div>
						{#if result.state.value.series.length > 1}<p class="font-mono text-xs text-muted-foreground">{name(s)}</p>{/if}
						{@render current(s, true)}
					</div>
				{/each}
			</div>
		{:else}
			<Table.Root>
				<Table.Header>
					<Table.Row><Table.Head>{t('colSeries')}</Table.Head><Table.Head>{t('colValue')}</Table.Head></Table.Row>
				</Table.Header>
				<Table.Body>
					{#each result.state.value.series as s, i (i)}
						<Table.Row>
							<Table.Cell class="font-mono text-xs">{name(s)}</Table.Cell>
							<Table.Cell>{@render current(s, false)}</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		{/if}
	</Card.Content>
</Card.Root>
