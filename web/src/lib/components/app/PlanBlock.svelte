<script lang="ts">
	import type { ServicePlan } from '../../api';
	import { planView } from '../../services';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';

	// Shows a service plan the way the manager returned it: ordered steps,
	// the files it would generate (never secret values), warnings and blockers.
	let { plan }: { plan: ServicePlan } = $props();
	const view = $derived(planView(plan));
</script>

<div class="space-y-3 text-sm">
	<p class="text-muted-foreground">{t('svcPlanIntro')}</p>
	{#if plan.text}<p class="whitespace-pre-line">{plan.text}</p>{/if}
	<div>
		<h4 class="mb-1 font-medium">{t('svcPlanSteps')}</h4>
		<ol class="list-decimal space-y-1 pl-5">
			{#each plan.steps as s (s.id)}
				<li>
					<span class="font-mono text-xs">{s.kind}</span>{#if s.slot} <span class="text-muted-foreground">({s.slot})</span>{/if}
					{#if s.note}<span class="text-muted-foreground"> — {s.note}</span>{/if}
					{#if s.conditions?.length}
						<ul class="list-disc pl-5 text-xs text-muted-foreground">{#each s.conditions as c (c)}<li>{c}</li>{/each}</ul>
					{/if}
				</li>
			{/each}
		</ol>
	</div>
	{#if plan.artifacts.length > 0}
		<div class="space-y-2">
			<h4 class="font-medium">{t('svcPlanArtifacts')}</h4>
			{#each plan.artifacts as a (a.name)}
				<div>
					<p class="font-mono text-xs text-muted-foreground">{a.name} <span class="opacity-70">({a.mediaType})</span></p>
					<pre class="max-h-64 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-xs"><code>{a.content}</code></pre>
				</div>
			{/each}
		</div>
	{/if}
	{#if plan.warnings.length > 0}
		<Alert.Root>
			<Alert.Title>{t('svcPlanWarnings')}</Alert.Title>
			<Alert.Description><ul class="list-disc pl-5">{#each plan.warnings as w (w)}<li>{w}</li>{/each}</ul></Alert.Description>
		</Alert.Root>
	{/if}
	{#if view.blocked}
		<Alert.Root variant="destructive">
			<Alert.Title>{t('svcPlanBlockers')}</Alert.Title>
			<Alert.Description class="space-y-2">
				<ul class="list-disc pl-5">{#each plan.blockers as b (b)}<li>{b}</li>{/each}</ul>
				<p>{t('svcBlockedNote')}</p>
			</Alert.Description>
		</Alert.Root>
	{/if}
</div>
