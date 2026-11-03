<script lang="ts">
	import { api, asApiError, type ApiError, type PatchHostSummary, type PatchPlan } from '../../api';
	import { load } from '../../load.svelte';
	import { locale, t } from '../../locale.svelte';
	import { dateTime } from '../../format';
	import ApiGate from './ApiGate.svelte';
	import Loading from './Loading.svelte';
	import { Button } from '../ui/button/index.js';
	import * as Table from '../ui/table/index.js';

	let { hosts }: { hosts: PatchHostSummary[] } = $props();
	const plans = load(api.patchPlans);
	let hostId = $state('');
	let securityOnly = $state(true);
	let reboot = $state(false);
	let busy = $state('');
	let error = $state<ApiError | undefined>();

	const names = $derived(new Map(hosts.map((h) => [h.host.id, h.host.name])));

	async function run(key: string, fn: () => Promise<unknown>) {
		busy = key; error = undefined;
		try { await fn(); plans.refresh(); }
		catch (e) { error = asApiError(e); }
		finally { busy = ''; }
	}
	const propose = () => run('propose', () => api.proposePatchPlan(hostId, { securityOnly, reboot }));
	const act = (p: PatchPlan, what: 'approve' | 'decline' | 'apply') =>
		run(p.id + what, () => (what === 'approve' ? api.approvePatchPlan(p.id, p.digest) : what === 'decline' ? api.declinePatchPlan(p.id) : api.applyPatchPlan(p.id)));
</script>

<section class="mt-8 space-y-3">
	<h2 class="text-lg font-medium">{t('patchPlans')}</h2>
	<p class="text-sm text-muted-foreground">{t('patchPlansIntro')}</p>
	<div class="flex flex-wrap items-center gap-3">
		<select class="h-9 rounded-md border bg-background px-2 text-sm" aria-label={t('patchHost')} bind:value={hostId}>
			<option value="">{t('patchChooseHost')}</option>
			{#each hosts as h (h.host.id)}<option value={h.host.id}>{h.host.name}</option>{/each}
		</select>
		<label class="flex items-center gap-1 text-sm"><input type="checkbox" bind:checked={securityOnly} />{t('patchSecurityOnly')}</label>
		<label class="flex items-center gap-1 text-sm"><input type="checkbox" bind:checked={reboot} />{t('patchWithReboot')}</label>
		<Button size="sm" disabled={!hostId || busy !== ''} onclick={propose}>{t('patchPropose')}</Button>
	</div>
	{#if error}<p class="text-sm text-destructive" role="alert">{error.message}</p>{/if}
	{#if plans.state.status === 'loading'}<Loading />
	{:else if plans.state.status === 'error'}<ApiGate error={plans.state.error} retry={plans.reload} />
	{:else if !plans.state.value.length}<p class="text-sm text-muted-foreground">{t('patchNoPlans')}</p>
	{:else}
		<div class="overflow-x-auto">
		<Table.Root>
			<Table.Header><Table.Row>
				<Table.Head>{t('patchHost')}</Table.Head><Table.Head>{t('patchPlanPackages')}</Table.Head><Table.Head>{t('patchPlanState')}</Table.Head><Table.Head>{t('patchReported')}</Table.Head><Table.Head></Table.Head>
			</Table.Row></Table.Header>
			<Table.Body>
				{#each plans.state.value as p (p.id)}
					<Table.Row>
						<Table.Cell>{names.get(p.hostId) ?? p.hostId}</Table.Cell>
						<Table.Cell><div class="text-sm">{p.packages.length ? p.packages.join(', ') : '—'}</div>
							<div class="text-xs text-muted-foreground">{#if p.securityOnly}{t('patchSecurityOnly')} · {/if}{#if p.reboot}{t('patchWithReboot')} · {/if}{#if p.containerRuntime}{t('patchRuntimeClass')} · {/if}<span class="font-mono" title={p.digest}>{p.digest.slice(0, 12)}</span></div></Table.Cell>
						<Table.Cell>{t(`patchPlan_${p.state}`)}{#if p.error}<div class="text-xs text-destructive">{p.error}</div>{/if}</Table.Cell>
						<Table.Cell>{dateTime(p.createdAt, locale.value) ?? '—'}</Table.Cell>
						<Table.Cell class="space-x-2 whitespace-nowrap">
							{#if p.state === 'proposed'}<Button size="sm" disabled={busy !== ''} onclick={() => act(p, 'approve')}>{t('patchApprove')}</Button>{/if}
							{#if p.state === 'approved'}<Button size="sm" disabled={busy !== ''} onclick={() => act(p, 'apply')}>{t('patchApply')}</Button>{/if}
							{#if p.state === 'proposed' || p.state === 'approved'}<Button size="sm" variant="outline" disabled={busy !== ''} onclick={() => act(p, 'decline')}>{t('patchDecline')}</Button>{/if}
						</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
		</div>
	{/if}
</section>
