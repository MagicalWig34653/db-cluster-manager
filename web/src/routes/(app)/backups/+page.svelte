<script lang="ts">
	import StateView from '../../../lib/components/app/StateView.svelte';

	import { api, asApiError, type ApiError, type InspectionStarted, type PointInTimeProposal } from '../../../lib/api';
	import { restoreHosts } from '../../../lib/operations';
	import { formatUtc, pitrReady, utcInputToRfc3339 } from '../../../lib/pitr';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import { checkClass, selectClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import BackupControls from '../../../lib/components/app/BackupControls.svelte';
	import OperationCard from '../../../lib/components/app/OperationCard.svelte';
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

	// Restore wizard: select cluster and host -> start the inspection restore
	// (POST /restores/inspect) -> review, copy the one-time password and approve
	// the displayed plan (POST /operations/{id}/approve with its digest).
	// Free disk is not observed, so the host is always chosen explicitly.
	const hosts = load(api.hosts);
	const ops = load(api.operations);
	let step = $state(0);
	let cluster = $state('');
	let host = $state('');
	let confirmed = $state(false);
	let inspecting = $state(false);
	let started = $state<InspectionStarted | undefined>();
	// The password lives only in this variable and is dropped once the user confirms having saved it.
	let password = $state<string | undefined>();
	let saved = $state(false);
	let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
	let inspectError = $state<ApiError | undefined>();
	const swarmOf = $derived(clusters.state.status === 'ok' ? clusters.state.value.find((c) => c.id === cluster)?.swarm : undefined);
	const candidates = $derived(hosts.state.status === 'ok' ? restoreHosts(hosts.state.value, swarmOf) : []);
	const hostName = (id: string) => (hosts.state.status === 'ok' ? hosts.state.value.find((h) => h.nodeId === id)?.hostname : undefined) ?? id;
	const op = $derived(started && ops.state.status === 'ok' ? ops.state.value.find((o) => o.id === started!.id) : undefined);
	// Restore point: the newest backup, or a time (UTC) the archived binary log
	// can reach. The coverage is checked read-only (GET /clusters/{id}/pitr-proposal)
	// before the restore may start; it still goes to a new isolated instance and
	// waits for the approval like every inspection.
	let mode = $state<'latest' | 'time'>('latest');
	let atInput = $state('');
	let atTouched = $state(false);
	let proposal = $state<PointInTimeProposal | undefined>();
	let proposing = $state(false);
	let proposalError = $state<ApiError | undefined>();
	const at = $derived(utcInputToRfc3339(atInput));
	const timeReady = $derived(mode === 'latest' || pitrReady(at, proposal));
	const stale = $derived(proposal !== undefined && at !== undefined && Date.parse(proposal.at) !== Date.parse(at));
	function pickCluster(id: string) {
		cluster = id;
		host = '';
		proposal = undefined;
	}
	async function check() {
		atTouched = true;
		if (!cluster || !at) return;
		proposing = true;
		proposalError = undefined;
		try {
			proposal = await api.pitrProposal(cluster, at);
		} catch (e) {
			proposal = undefined;
			proposalError = asApiError(e);
		} finally {
			proposing = false;
		}
	}
	async function inspect() {
		if (!cluster || !host || !timeReady) return;
		inspecting = true;
		inspectError = undefined;
		try {
			started = await api.inspectRestore({ cluster, host, ...(mode === 'time' && at ? { pointInTime: at } : {}) });
			password = started.inspectionPassword;
			saved = false;
			copyState = 'idle';
			ops.refresh();
			step = 2;
		} catch (e) {
			inspectError = asApiError(e);
		} finally {
			inspecting = false;
		}
	}
	async function copy() {
		try {
			await navigator.clipboard.writeText(password ?? '');
			copyState = 'copied';
		} catch {
			copyState = 'failed';
		}
	}
	function confirmSaved() {
		saved = true;
		password = undefined;
		copyState = 'idle';
	}
	function again() {
		started = undefined;
		password = undefined;
		confirmed = false;
		host = '';
		step = 0;
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
		<StateView kind="empty" message="clustersEmpty" />
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
						<Table.Head>{t('colNextRun')}</Table.Head>
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
							<Table.Cell>{#if b?.nextRunAt}<Freshness at={b.nextRunAt} />{:else}{t('notReported')}{/if}</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
		<Card.Root>
			<Card.Header><Card.Title><h2>{t('backupManage')}</h2></Card.Title></Card.Header>
			<Card.Content class="grid gap-6 md:grid-cols-2">
				{#each clusters.state.value as c (c.id)}
					<BackupControls cluster={c.id} name={c.name} summary={summary(c.id)} onchange={backups.refresh} />
				{/each}
			</Card.Content>
		</Card.Root>
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
					<select id="rs-cluster" class={selectClass} value={cluster} onchange={(e) => pickCluster(e.currentTarget.value)} disabled={clusters.state.status !== 'ok'}>
						<option value="">{t('choose')}</option>
						{#if clusters.state.status === 'ok'}{#each clusters.state.value as c (c.id)}<option value={c.id}>{c.name}</option>{/each}{/if}
					</select>
				</div>
				<div class="space-y-1.5">
					<Label for="rs-host">{t('restoreHost')}</Label>
					{#if hosts.state.status === 'loading'}
						<Loading />
					{:else if hosts.state.status === 'error'}
						<ApiGate error={hosts.state.error} retry={hosts.reload} />
					{:else}
						<select id="rs-host" class={selectClass} bind:value={host} disabled={!cluster || candidates.length === 0} aria-describedby="rs-host-hint">
							<option value="">{t('choose')}</option>
							{#each candidates as h (h.nodeId)}<option value={h.nodeId}>{h.hostname ?? h.nodeId}</option>{/each}
						</select>
						<p id="rs-host-hint" class="text-xs text-muted-foreground">{cluster && candidates.length === 0 ? t('restoreNoHost') : t('restoreHostHint')}</p>
					{/if}
				</div>
				<fieldset class="space-y-2" disabled={!cluster}>
					<legend class="text-sm font-medium">{t('restorePoint')}</legend>
					<label class="flex items-center gap-2 text-sm"><input type="radio" name="rs-mode" value="latest" bind:group={mode} />{t('restorePointLatest')}</label>
					<label class="flex items-center gap-2 text-sm"><input type="radio" name="rs-mode" value="time" bind:group={mode} />{t('restorePointTime')}</label>
					{#if mode === 'time'}
						<div class="space-y-1.5 pl-6">
							<Label for="rs-at">{t('pitrAt')}</Label>
							<div class="flex flex-wrap items-center gap-2">
								<Input id="rs-at" type="datetime-local" step="1" class="w-auto font-mono" bind:value={atInput} aria-invalid={atTouched && !at} aria-describedby="rs-at-hint" />
								<Button type="button" variant="outline" disabled={!cluster || !at || proposing} onclick={check}>{t('pitrCheck')}</Button>
							</div>
							<p id="rs-at-hint" class="text-xs {atTouched && !at ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{atTouched && !at ? t('pitrInvalidTime') : t('pitrAtHint')}</p>
							<div aria-live="polite" class="space-y-2">
								{#if proposalError}<ErrorNotice error={proposalError} />{/if}
								{#if stale}<p class="text-sm">{t('pitrStale')}</p>{/if}
								{#if proposal && !stale}
									<Alert.Root variant={proposal.covered ? 'default' : 'destructive'}>
										<Alert.Title>{proposal.covered ? t('pitrCoveredTitle') : t('pitrNotCoveredTitle')}</Alert.Title>
										<Alert.Description class="space-y-2">
											{#if !proposal.covered && proposal.reason}<p>{proposal.reason}</p>{/if}
											<dl class="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1">
												{#if proposal.backup}
													<dt class="text-muted-foreground">{t('pitrBackup')}</dt>
													<dd class="font-mono text-xs">{proposal.backup}{#if proposal.backupFinishedAt} ({formatUtc(proposal.backupFinishedAt)}){/if}</dd>
												{/if}
												{#if proposal.covered}
													<dt class="text-muted-foreground">{t('pitrSegments')}</dt><dd>{proposal.segments ?? 0}</dd>
												{/if}
												{#if proposal.coverage}
													<dt class="text-muted-foreground">{t('pitrCoverage')}</dt>
													<dd>{proposal.coverage.until ? t('pitrCoverageRange', { from: formatUtc(proposal.coverage.from), until: formatUtc(proposal.coverage.until) }) : t('pitrCoverageNone')}</dd>
												{/if}
											</dl>
											{#if proposal.coverage?.gap}<p>{t('pitrGap', { gap: proposal.coverage.gap })}</p>{/if}
										</Alert.Description>
									</Alert.Root>
								{/if}
							</div>
						</div>
					{/if}
				</fieldset>
				<Button disabled={!cluster || !host || !timeReady} onclick={() => (step = 1)}>{t('continue')}</Button>
			{:else if step === 1}
				<p class="text-sm text-muted-foreground">{t('inspectHint')}</p>
				<p class="text-sm">{t('inspectionHost')}: <span class="font-medium">{hostName(host)}</span></p>
				{#if mode === 'time' && at}
					<p class="text-sm">{t('inspectionPointInTime')}: <span class="font-medium">{formatUtc(at)}</span></p>
					<p class="text-sm text-muted-foreground">{t('pitrNewInstance')}</p>
				{/if}
				<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={confirmed} />{t('restoreUnderstand')}</label>
				<div class="flex gap-2">
					<Button variant="outline" onclick={() => (step = 0)}>{t('back')}</Button>
					<Button disabled={inspecting || !confirmed} onclick={inspect}>{t('runInspection')}</Button>
				</div>
				{#if inspectError}<div aria-live="polite"><ErrorNotice error={inspectError} /></div>{/if}
			{:else if started}
				<h3 class="text-sm font-medium">{t('inspectionResult')}</h3>
				<p class="text-sm">{t('restoreStarted', { id: started.id })}</p>
				<dl class="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
					<dt class="text-muted-foreground">{t('inspectionBackup')}</dt><dd class="font-mono text-xs">{started.backup}</dd>
					{#if started.pointInTime}<dt class="text-muted-foreground">{t('inspectionPointInTime')}</dt><dd>{formatUtc(started.pointInTime)}</dd>{/if}
					<dt class="text-muted-foreground">{t('inspectionHost')}</dt><dd>{hostName(started.host)}</dd>
					<dt class="text-muted-foreground">{t('inspectionService')}</dt><dd class="font-mono text-xs">{started.service}:{started.port}</dd>
					<dt class="text-muted-foreground">{t('inspectionAccount')}</dt><dd class="font-mono text-xs">{started.inspectionAccount}</dd>
					<dt class="text-muted-foreground">{t('inspectionExpires')}</dt><dd><Freshness at={started.expiresAt} /></dd>
				</dl>
				{#if password !== undefined}
					<Alert.Root variant="destructive">
						<Alert.Title>{t('inspectionPassword')}</Alert.Title>
						<Alert.Description class="space-y-2">
							<p>{t('inspectionPasswordWarn')}</p>
							<div class="flex flex-wrap items-center gap-2">
								<code class="rounded bg-muted px-2 py-1 font-mono text-sm break-all select-all">{password}</code>
								<Button size="sm" variant="outline" onclick={copy}>{copyState === 'copied' ? t('copied') : t('copy')}</Button>
							</div>
							{#if copyState === 'failed'}<p role="status">{t('copyFailed')}</p>{/if}
							<Button size="sm" onclick={confirmSaved}>{t('inspectionSaved')}</Button>
						</Alert.Description>
					</Alert.Root>
				{:else if saved}
					<p class="text-sm text-muted-foreground">{t('inspectionPasswordGone')}</p>
				{/if}
				<Alert.Root><Alert.Description>{t('restoreApprovalNote')}</Alert.Description></Alert.Root>
				<Alert.Root><Alert.Description>{t('restoreVerifyNote')}</Alert.Description></Alert.Root>
				<!-- The approval is offered by the operation view: only a digest requirement enables it. -->
				{#if ops.state.status === 'error'}
					<ApiGate error={ops.state.error} retry={ops.reload} />
				{:else if op}
					<div class="rounded-md border p-3"><OperationCard {op} onchange={ops.refresh} /></div>
				{:else}
					<Loading />
				{/if}
				<div class="flex gap-2">
					<Button variant="outline" onclick={again}>{t('back')}</Button>
					<Button variant="ghost" href="/operations#{encodeURIComponent(started.id)}">{t('navOperations')}</Button>
				</div>
			{/if}
		</Card.Content>
	</Card.Root>
</div>
