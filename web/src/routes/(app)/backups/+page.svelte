<script lang="ts">
	import { api, asApiError, type ApiError } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import { checkClass, selectClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	// Clusters exist today; backup summaries (assumed GET /api/v1/backups) may not.
	const clusters = load(api.clusters);
	const backups = load(api.backups);
	const summary = (cluster: string) =>
		backups.state.status === 'ok' ? backups.state.value.find((b) => b.cluster === cluster) : undefined;

	// Restore wizard skeleton: select -> inspect (read-only) -> approve. Nothing
	// is restored by this UI; the approval step has no endpoint yet.
	let step = $state(0);
	let cluster = $state('');
	let confirmed = $state(false);
	let inspecting = $state(false);
	let inspection = $state<Record<string, unknown> | undefined>();
	let inspectError = $state<ApiError | undefined>();
	async function inspect() {
		inspecting = true;
		inspectError = undefined;
		try {
			inspection = await api.inspectRestore({ cluster });
			step = 2;
		} catch (e) {
			inspectError = asApiError(e);
		} finally {
			inspecting = false;
		}
	}
	const steps = ['restoreSelect', 'restoreInspect', 'restoreApprove'] as const;
</script>

<PageHeader title={t('navBackups')} intro={t('backupsIntro')} />

<div class="space-y-6">
	{#if clusters.state.status === 'loading'}
		<Loading />
	{:else if clusters.state.status === 'error'}
		<ErrorNotice error={clusters.state.error} retry={clusters.reload} />
	{:else if clusters.state.value.length === 0}
		<p class="text-sm text-muted-foreground">{t('clustersEmpty')}</p>
	{:else}
		{#if backups.state.status === 'error'}<ApiGate error={backups.state.error} retry={backups.reload} />{/if}
		<div class="rounded-lg border">
			<Table.Root>
				<Table.Header>
					<Table.Row>
						<Table.Head>{t('navClusters')}</Table.Head>
						<Table.Head>{t('backupPolicy')}</Table.Head>
						<Table.Head>{t('lastBackup')}</Table.Head>
						<Table.Head>{t('lastVerifiedRestore')}</Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each clusters.state.value as c (c.id)}
						{@const b = summary(c.id)}
						<Table.Row>
							<Table.Cell class="font-medium">{c.name}</Table.Cell>
							<Table.Cell>{b?.policy ?? c.automationPolicy ?? t('notReported')}</Table.Cell>
							<Table.Cell>{#if b?.lastBackupAt ?? c.lastBackupAt}<Freshness at={b?.lastBackupAt ?? c.lastBackupAt} />{:else}{t('stateUnknown')}{/if}</Table.Cell>
							<Table.Cell>
								{#if b?.lastVerifiedRestoreAt ?? c.lastVerifiedRestoreAt}<Freshness at={b?.lastVerifiedRestoreAt ?? c.lastVerifiedRestoreAt} />{:else}{t('neverVerifiedUnknown')}{/if}
							</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
	{/if}

	<Card.Root>
		<Card.Header>
			<Card.Title><h2>{t('restoreWizard')}</h2></Card.Title>
			<Card.Description>{t('restoreIntro')}</Card.Description>
		</Card.Header>
		<Card.Content class="space-y-4">
			<ol class="flex flex-wrap gap-2 text-sm" aria-label={t('wizardSteps')}>
				{#each steps as s, i (s)}
					<li aria-current={i === step ? 'step' : undefined} class={i === step ? 'font-semibold' : 'text-muted-foreground'}>{i + 1}. {t(s)}{i < 2 ? ' ›' : ''}</li>
				{/each}
			</ol>
			{#if step === 0}
				<div class="space-y-1.5">
					<Label for="rs-cluster">{t('navClusters')}</Label>
					<select id="rs-cluster" class={selectClass} bind:value={cluster} disabled={clusters.state.status !== 'ok'}>
						<option value="">{t('choose')}</option>
						{#if clusters.state.status === 'ok'}{#each clusters.state.value as c (c.id)}<option value={c.id}>{c.name}</option>{/each}{/if}
					</select>
				</div>
				<Button disabled={!cluster} onclick={() => (step = 1)}>{t('continue')}</Button>
			{:else if step === 1}
				<p class="text-sm text-muted-foreground">{t('inspectHint')}</p>
				<div class="flex gap-2">
					<Button variant="outline" onclick={() => (step = 0)}>{t('back')}</Button>
					<Button disabled={inspecting} onclick={inspect}>{t('runInspection')}</Button>
				</div>
				{#if inspectError}<div aria-live="polite"><ApiGate error={inspectError} /></div>{/if}
			{:else}
				<h3 class="text-sm font-medium">{t('inspectionResult')}</h3>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs">{JSON.stringify(inspection, null, 2)}</pre>
				<Alert.Root><Alert.Description>{t('restoreApprovalNote')}</Alert.Description></Alert.Root>
				<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={confirmed} />{t('restoreUnderstand')}</label>
				<div class="flex gap-2">
					<Button variant="outline" onclick={() => (step = 1)}>{t('back')}</Button>
					<!-- No approval endpoint exists; this stays disabled and says so. -->
					<Button disabled>{t('restoreApproveUnavailable')}</Button>
				</div>
			{/if}
		</Card.Content>
	</Card.Root>
</div>
