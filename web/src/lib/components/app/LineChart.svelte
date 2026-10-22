<script lang="ts">
	import type { MetricSeries } from '../../api';
	import { buildChart, pathOf } from '../../chart';
	import { locale, t } from '../../locale.svelte';

	let {
		series,
		from,
		to,
		step,
		label,
		unit = ''
	}: { series: MetricSeries; from: number; to: number; step: number; label: string; unit?: string } = $props();

	const W = 640;
	const H = 220;
	const pad = { l: 48, r: 12, t: 10, b: 24 };
	const chart = $derived(buildChart({ series, from, to, step, width: W, height: H, pad }));
	const fmt = (v: number) => new Intl.NumberFormat(locale.value, { maximumFractionDigits: 2 }).format(v);
	const fmtTime = (s: number) =>
		new Intl.DateTimeFormat(locale.value, { hour: '2-digit', minute: '2-digit' }).format(new Date(s * 1000));
	const summary = $derived(
		chart.hasData
			? t('chartSummary', { label, points: chart.valuePoints, min: fmt(chart.yMin), max: fmt(chart.yMax), gaps: chart.gaps.length })
			: t('chartNoData', { label })
	);
</script>

<!-- Colors come from tokens; the line also differs from gaps by pattern, not only color. -->
<svg viewBox="0 0 {W} {H}" class="h-auto w-full text-primary" role="img" aria-label={summary}>
	<defs>
		<pattern id="gap-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
			<line x1="0" y1="0" x2="0" y2="6" stroke="currentColor" stroke-width="1.5" class="text-muted-foreground" opacity="0.5" />
		</pattern>
	</defs>
	{#each chart.yTicks as tick (tick.value)}
		<line x1={pad.l} x2={W - pad.r} y1={tick.y} y2={tick.y} class="stroke-border" stroke-width="1" />
		<text x={pad.l - 6} y={tick.y} text-anchor="end" dominant-baseline="middle" class="fill-muted-foreground text-[10px]">{fmt(tick.value)}{unit}</text>
	{/each}
	{#each chart.xTicks as tick (tick.time)}
		<text x={tick.x} y={H - 6} text-anchor="middle" class="fill-muted-foreground text-[10px]">{fmtTime(tick.time)}</text>
	{/each}
	{#each chart.gaps as g, i (i)}
		<rect x={g.x1} y={pad.t} width={Math.max(g.x2 - g.x1, 1)} height={H - pad.t - pad.b} fill="url(#gap-hatch)">
			<title>{t('chartGap')}</title>
		</rect>
	{/each}
	{#each chart.runs as run, i (i)}
		{#if run.length === 1}
			<circle cx={run[0].x} cy={run[0].y} r="3" fill="currentColor" />
		{:else}
			<path d={pathOf(run)} fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
		{/if}
	{/each}
</svg>
<p class="mt-1 flex flex-wrap items-center gap-x-3 text-xs text-muted-foreground">
	<span>{summary}</span>
	{#if chart.gaps.length > 0}
		<span class="inline-flex items-center gap-1">
			<svg width="14" height="10" aria-hidden="true"><rect width="14" height="10" fill="url(#gap-hatch)" class="text-muted-foreground" /></svg>{t('chartGap')}
		</span>
	{/if}
</p>
