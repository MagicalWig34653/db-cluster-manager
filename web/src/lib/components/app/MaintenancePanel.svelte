<script lang="ts">
	import { api, asApiError, type ApiError, type Host, type MaintenancePreview, type MaintenanceStarted } from '../../api';
	import { dateTime } from '../../format';
	import { maintenanceState, maintenanceView } from '../../platform';
	import { locale, t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import ApiGate from './ApiGate.svelte';
	import FollowOperation from './FollowOperation.svelte';

	// Guided maintenance of one host: preview the plan (read-only), then start
	// or end it. The manager never reboots or drains the host.
	let { host, onchange }: { host: Host; onchange: () => void } = $props();

	const end = $derived(maintenanceState(host) !== 'none');
	let plan = $state<MaintenancePreview | undefined>();
	let planError = $state<ApiError | undefined>();
	let reason = $state('');
	let busy = $state(false);
	let started = $state<MaintenanceStarted | undefined>();
	let error = $state<ApiError | undefined>();
	const view = $derived(plan ? maintenanceView(plan) : undefined);

	async function show() {
		busy = true;
		planError = undefined;
		started = undefined;
		try {
			plan = await api.maintenancePlan(host.nodeId, end);
		} catch (e) {
			plan = undefined;
			planError = asApiError(e);
		} finally {
			busy = false;
		}
	}
	async function run() {
		busy = true;
		error = undefined;
		try {
			started = end ? await api.leaveMaintenance(host.nodeId) : await api.enterMaintenance(host.nodeId, reason.trim() || undefined);
			onchange();
		} catch (e) {
			error = asApiError(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="space-y-3 rounded-md border bg-muted/30 p-3 text-sm" aria-label={t('mntTitle', { host: host.hostname ?? host.nodeId })}>
	<h3 class="font-medium">{t('mntTitle', { host: host.hostname ?? host.nodeId })}</h3>
	<p class="text-muted-foreground">{t('mntIntro')}</p>
	{#if host.maintenance}
		<Alert.Root>
			<Alert.Title>{maintenanceState(host) === 'ready' ? t('mntReady') : t('mntInProgress')}</Alert.Title>
			<Alert.Description>{t('mntSince', { at: dateTime(host.maintenance.since, locale.value) ?? host.maintenance.since, by: host.maintenance.by })}{#if host.maintenance.reason}: {host.maintenance.reason}{/if}</Alert.Description>
		</Alert.Root>
	{/if}
	<Button size="sm" variant="outline" disabled={busy} onclick={show}>{end ? t('mntShowEndPlan') : t('mntShowPlan')}</Button>
	<div aria-live="polite" class="space-y-3">
		{#if planError}<ApiGate error={planError} />{/if}
		{#if plan && view}
			{#if view.empty}
				<p>{t('mntEmpty')}</p>
			{:else}
				<ul class="space-y-2">
					{#each plan.clusters as c (c.cluster)}
						<li class="rounded-md border p-2">
							<p class="font-medium">{t('mntCluster', { name: c.name, slots: c.slots.join(', ') })}</p>
							{#if c.refused}
								<p class="mt-1 text-red-700 dark:text-red-400">{c.refused}</p>
							{:else}
								<ol class="mt-1 list-decimal space-y-0.5 pl-5">
									{#each c.steps as s (s.id)}
										<li>
											<span class="font-mono text-xs">{s.kind}</span>{#if s.slot} <span class="text-muted-foreground">({s.slot})</span>{/if}
											{#if s.note}<span class="text-muted-foreground"> — {s.note}</span>{/if}
											{#if s.conditions?.length}<ul class="list-disc pl-5 text-xs text-muted-foreground">{#each s.conditions as cond (cond)}<li>{cond}</li>{/each}</ul>{/if}
										</li>
									{/each}
								</ol>
							{/if}
							{#each c.warnings ?? [] as w (w)}<p class="mt-1 text-xs text-amber-700 dark:text-amber-400">{w}</p>{/each}
						</li>
					{/each}
				</ul>
				<p class="text-xs text-muted-foreground">{t('mntStepCount', { count: view.stepCount })}</p>
			{/if}
			{#if view.canStart}
				{#if !end && !view.empty}<p class="font-medium text-emerald-700 dark:text-emerald-400">{t('mntSafe')}</p>{/if}
				{#if !started}
					{#if !end}
						<div class="max-w-md space-y-1.5">
							<Label for="mn-reason-{host.nodeId}">{t('mntReason')}</Label>
							<Input id="mn-reason-{host.nodeId}" bind:value={reason} maxlength={500} autocomplete="off" />
						</div>
					{/if}
					<Button size="sm" disabled={busy} onclick={run}>{end ? t('mntEnd') : t('mntStart')}</Button>
				{/if}
			{:else}
				<Alert.Root variant="destructive">
					<Alert.Title>{t('mntRefusedTitle')}</Alert.Title>
					<Alert.Description>{t('mntRefusedHint')}</Alert.Description>
				</Alert.Root>
			{/if}
		{/if}
		{#if error}<ApiGate {error} />{/if}
		{#if started}
			<Alert.Root><Alert.Description>{end ? t('mntEnded', { count: started.operations.length }) : t('mntStarted', { count: started.operations.length })}</Alert.Description></Alert.Root>
			{#each started.operations as o (o.id)}<FollowOperation id={o.id} />{/each}
		{/if}
	</div>
</section>
