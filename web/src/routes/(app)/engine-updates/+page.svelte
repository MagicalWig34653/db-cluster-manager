<script lang="ts">
	import { api, asApiError, type ApiError, type EngineUpdatePlan } from '../../../lib/api';
	import { canExportEnginePlan, engineRunbookFilename } from '../../../lib/engine-updates';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { selectClass } from '../../../lib/components/app/fieldClass';

	const hosts = load(api.hosts);
	let selectedHost = $state('');
	let target = $state('');
	let plan = $state<EngineUpdatePlan | undefined>();
	let error = $state<ApiError | undefined>();
	let busy = $state(false);
	let confirmedManager = $state(false);

	async function refreshPlan(nextTarget = target) {
		if (!selectedHost) return;
		busy = true; error = undefined; plan = undefined;
		try { plan = await api.engineUpdatePlan(selectedHost, nextTarget || undefined); }
		catch (e) { error = asApiError(e); }
		finally { busy = false; }
	}
	function selectHost(id: string) {
		selectedHost = id; target = ''; plan = undefined; confirmedManager = false; error = undefined;
		if (id) void refreshPlan('');
	}
	function selectTarget(version: string) {
		target = version; plan = undefined; confirmedManager = false;
		if (version) void refreshPlan(version);
	}
	async function download() {
		if (!canExportEnginePlan(plan, target, confirmedManager) || !plan) return;
		busy = true; error = undefined;
		try {
			const book = await api.exportEngineRunbook(selectedHost, { targetVersion: target, digest: plan.digest, confirmPinnedManager: confirmedManager });
			const url = URL.createObjectURL(new Blob([book.script], { type: 'text/x-shellscript' }));
			const a = document.createElement('a'); a.href = url; a.download = engineRunbookFilename(selectedHost);
			document.body.appendChild(a); a.click(); a.remove();
			setTimeout(() => URL.revokeObjectURL(url), 60_000);
		} catch (e) { error = asApiError(e); plan = undefined; }
		finally { busy = false; }
	}
</script>

<PageHeader title={t('engineTitle')} intro={t('engineIntro')} />
<div class="max-w-4xl space-y-5">
	{#if hosts.state.status === 'loading'}<Loading />
	{:else if hosts.state.status === 'error'}<ApiGate error={hosts.state.error} retry={hosts.reload} />
	{:else}
		<div class="grid gap-4 sm:grid-cols-2">
			<label class="space-y-1 text-sm"><span>{t('engineHost')}</span><select class={selectClass} value={selectedHost} onchange={(e) => selectHost(e.currentTarget.value)}><option value="">—</option>{#each hosts.state.value as h (h.swarm + h.nodeId)}<option value={h.nodeId}>{h.hostname ?? h.nodeId} ({h.swarm})</option>{/each}</select></label>
			<label class="space-y-1 text-sm"><span>{t('engineTarget')}</span><select class={selectClass} value={target} disabled={!plan?.availableVersions.length || busy} onchange={(e) => selectTarget(e.currentTarget.value)}><option value="">—</option>{#each plan?.availableVersions ?? [] as v (v)}<option value={v}>{v}</option>{/each}</select></label>
		</div>
		{#if selectedHost}<Button variant="outline" disabled={busy} onclick={() => refreshPlan()}>{t('engineShowPlan')}</Button>{/if}
		{#if busy}<Loading />{/if}
		{#if error}<ApiGate {error} retry={() => refreshPlan()} />{/if}
		{#if plan}
			<section class="space-y-3 rounded-lg border p-4" aria-label={t('engineTitle')}>
				<p class="text-sm">{t('engineCurrent')}: <strong>{plan.currentVersion}</strong></p>
				{#if !plan.availableVersions.length}<p class="text-sm text-amber-700 dark:text-amber-400">{t('engineNoVersions')}</p>{/if}
				{#if plan.compatibility}<p class="text-sm">{t('engineCompatibility')}: {plan.compatibility}</p>{/if}
				<p class="break-all font-mono text-xs">{t('engineDigest')}: {plan.digest}</p>
				<h2 class="font-medium">{t('engineSteps')}</h2><ol class="list-decimal space-y-1 pl-5 text-sm">{#each plan.steps as step (step)}<li>{step}</li>{/each}</ol>
				{#if plan.warnings.length}<h2 class="font-medium">{t('engineWarnings')}</h2><ul class="list-disc pl-5 text-sm">{#each plan.warnings as warning (warning)}<li>{warning}</li>{/each}</ul>{/if}
				{#if plan.blockers.length}<h2 class="font-medium text-destructive">{t('engineBlockers')}</h2><ul class="list-disc pl-5 text-sm text-destructive">{#each plan.blockers as blocker (blocker)}<li>{blocker}</li>{/each}</ul>{/if}
				<h2 class="font-medium">{t('engineRollback')}</h2><ul class="list-disc pl-5 text-sm">{#each plan.rollbackNotes as note (note)}<li>{note}</li>{/each}</ul>
				{#if plan.pinnedManager}<label class="flex gap-2 text-sm"><input type="checkbox" bind:checked={confirmedManager} />{t('engineConfirmManager')}</label>{/if}
				<Button disabled={busy || !canExportEnginePlan(plan, target, confirmedManager)} onclick={download}>{t('engineExport')}</Button>
				<p class="text-xs text-muted-foreground">{t('engineExportHint')} {t('engineRefresh')}</p>
			</section>
		{/if}
	{/if}
</div>
