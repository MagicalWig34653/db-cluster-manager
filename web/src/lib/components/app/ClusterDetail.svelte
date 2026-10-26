<script lang="ts">
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import { api, asApiError, type ApiError, type Diagnosis, type OutageRecoveryStarted } from '../../api';
	import { canRequestRecovery, deleteConfirmed, eventsForCluster, highestSeqno, unknownPositions } from '../../lifecycle';
	import { dateTime, relativeAge } from '../../format';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import FollowOperation from './FollowOperation.svelte';
	import { load } from '../../load.svelte';
	import { locale, t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import * as Table from '../ui/table/index.js';
	import PageHeader from './PageHeader.svelte';
	import Status from './Status.svelte';
	import Freshness from './Freshness.svelte';
	import ErrorNotice from './ErrorNotice.svelte';
	import Loading from './Loading.svelte';
	import ApiGate from './ApiGate.svelte';
	import DriftPanel from './DriftPanel.svelte';
	import ClusterOperationPanel from './ClusterOperationPanel.svelte';
	import ProxyWriterPanel from './ProxyWriterPanel.svelte';
	import { Button } from '../ui/button/index.js';
	import * as Alert from '../ui/alert/index.js';

	let { id }: { id: string } = $props();
	// The parent re-creates this component when the id changes.
	// svelte-ignore state_referenced_locally
	const cluster = load(() => api.cluster(id));
	const events = load(api.events);

	// Assumed endpoints (not in openapi.yaml yet): POST /clusters/{id}/nodes/{slot}/replace
	// and GET /clusters/{id}/outage-diagnosis (galera.Diagnosis, advice only).
	let replacing = $state<string | undefined>(); // slot awaiting confirmation
	let replaceBusy = $state(false);
	let replaceResult = $state<{ slot: string; ok: true; op: string } | { slot: string; ok: false; error: ApiError } | undefined>();
	async function replace(slot: string) {
		replaceBusy = true;
		try {
			const r = await api.replaceNode(id, slot);
			replaceResult = { slot, ok: true, op: r.id };
		} catch (e) {
			replaceResult = { slot, ok: false, error: asApiError(e) };
		} finally {
			replaceBusy = false;
			replacing = undefined;
		}
	}
	let diag = $state<{ status: 'idle' } | { status: 'loading' } | { status: 'ok'; value: Diagnosis } | { status: 'error'; error: ApiError }>({ status: 'idle' });
	let recovering = $state(false);
	async function recoverPositions() {
		recovering = true;
		try {
			diag = { status: 'ok', value: await api.recoverOutagePositions(id) };
		} catch (e) {
			diag = { status: 'error', error: asApiError(e) };
		} finally {
			recovering = false;
		}
	}
	let planning = $state(false);
	let outagePlan = $state<{ status: 'ok'; value: OutageRecoveryStarted } | { status: 'error'; error: ApiError } | undefined>();
	async function requestRecovery(candidate: string) {
		planning = true;
		outagePlan = undefined;
		try {
			outagePlan = { status: 'ok', value: await api.requestOutageRecovery(id, candidate) };
		} catch (e) {
			outagePlan = { status: 'error', error: asApiError(e) };
		} finally {
			planning = false;
		}
	}
	let typed = $state('');
	let deleting = $state(false);
	let deletion = $state<{ status: 'ok'; id: string } | { status: 'error'; error: ApiError } | undefined>();
	async function planDelete() {
		deleting = true;
		deletion = undefined;
		try {
			deletion = { status: 'ok', id: (await api.deleteCluster(id)).id };
		} catch (e) {
			deletion = { status: 'error', error: asApiError(e) };
		} finally {
			deleting = false;
		}
	}
	async function diagnose() {
		diag = { status: 'loading' };
		try {
			diag = { status: 'ok', value: await api.outageDiagnosis(id) };
		} catch (e) {
			diag = { status: 'error', error: asApiError(e) };
		}
	}
</script>

{#snippet back()}
	<a href="/clusters" class="inline-flex items-center gap-1 text-sm text-muted-foreground underline-offset-4 hover:underline">
		<ArrowLeft class="size-4" aria-hidden="true" />{t('backToClusters')}
	</a>
{/snippet}

<!-- Values the API contract does not carry yet are rendered as unknown, never as healthy. -->
{#snippet notReported()}
	<span class="inline-flex flex-wrap items-center gap-x-2">
		<Status value="unknown" />
		<span class="text-xs text-muted-foreground">({t('notReported')})</span>
	</span>
{/snippet}

{#if cluster.state.status === 'loading'}
	<PageHeader title={t('clusterDetail')} />
	<Loading />
{:else if cluster.state.status === 'error'}
	<PageHeader title={t('clusterDetail')} />
	<div class="space-y-4">
		<ErrorNotice error={cluster.state.error} retry={cluster.reload} />
		{@render back()}
	</div>
{:else}
	{@const c = cluster.state.value}
	<div class="mb-3">{@render back()}</div>
	<PageHeader title={c.name}>
		{#snippet actions()}<Status value={c.state} class="text-base" />{/snippet}
	</PageHeader>

	<div class="grid gap-4 lg:grid-cols-[1fr_1fr]">
		<Card.Root>
			<Card.Content>
				<dl class="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-3 text-sm">
					<dt class="text-muted-foreground">{t('colState')}</dt>
					<dd class="flex flex-wrap items-center gap-x-3"><Status value={c.state} /><Freshness at={c.observedAt} /></dd>
					<dt class="text-muted-foreground">{t('colId')}</dt>
					<dd class="font-mono text-xs">{c.id}</dd>
					<dt class="text-muted-foreground">{t('colSwarm')}</dt>
					<dd class="font-mono text-xs">{c.swarm}</dd>
					<dt class="text-muted-foreground">{t('colEngine')}</dt>
					<dd>{#if c.engine}{c.engine}{:else}{@render notReported()}{/if}</dd>
					<dt class="text-muted-foreground">{t('endpoint')}</dt>
					<dd>
						{#if c.endpoints && c.endpoints.length > 0}
							<ul class="font-mono text-xs">{#each c.endpoints as e (e.host + e.port)}<li>{e.name ? e.name + ': ' : ''}{e.host}:{e.port}</li>{/each}</ul>
						{:else}{@render notReported()}{/if}
					</dd>
					<dt class="text-muted-foreground">{t('lastBackup')}</dt>
					<dd>{#if c.lastBackupAt}<Freshness at={c.lastBackupAt} />{:else}{@render notReported()}{/if}</dd>
					<dt class="text-muted-foreground">{t('lastVerifiedRestore')}</dt>
					<dd>{#if c.lastVerifiedRestoreAt}<Freshness at={c.lastVerifiedRestoreAt} />{:else}{@render notReported()}{/if}</dd>
					<dt class="text-muted-foreground">{t('automationPolicy')}</dt>
					<dd>{#if c.automationPolicy}{c.automationPolicy}{:else}{@render notReported()}{/if}</dd>
				</dl>
				<p class="mt-4 text-xs text-muted-foreground">{t('notReportedHint')}</p>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header><Card.Title><h2>{t('nodes')}</h2></Card.Title></Card.Header>
			<Card.Content>
				{#if c.nodes.length === 0}
					<p class="text-sm text-muted-foreground">{t('noNodes')}</p>
				{:else}
					<Table.Root>
						<Table.Header>
							<Table.Row>
								<Table.Head>{t('colId')}</Table.Head>
								<Table.Head>{t('colHost')}</Table.Head>
								<Table.Head>{t('colRole')}</Table.Head>
								<Table.Head>{t('colState')}</Table.Head>
								<Table.Head>{t('colFreshness')}</Table.Head>
								<Table.Head><span class="sr-only">{t('colActions')}</span></Table.Head>
							</Table.Row>
						</Table.Header>
						<Table.Body>
							{#each c.nodes as n (n.id)}
								<Table.Row>
									<Table.Cell class="font-mono text-xs">{n.id}</Table.Cell>
									<Table.Cell>{#if n.host}{n.host}{:else}<Status value="unknown" />{/if}</Table.Cell>
									<Table.Cell>{#if n.role}{n.role}{:else}<Status value="unknown" />{/if}</Table.Cell>
									<Table.Cell><Status value={n.state} /></Table.Cell>
									<Table.Cell><Freshness at={n.observedAt} /></Table.Cell>
									<Table.Cell>
										{#if replacing === n.id}
											<span class="flex flex-wrap gap-1">
												<Button size="sm" variant="destructive" disabled={replaceBusy} onclick={() => replace(n.id)}>{t('confirmReplace')}</Button>
												<Button size="sm" variant="outline" onclick={() => (replacing = undefined)}>{t('cancel')}</Button>
											</span>
										{:else}
											<Button size="sm" variant="outline" onclick={() => (replacing = n.id)}>{t('replaceNode')}</Button>
										{/if}
									</Table.Cell>
								</Table.Row>
							{/each}
						</Table.Body>
					</Table.Root>
				{/if}
				<div aria-live="polite" class="mt-3">
					{#if replaceResult}
						{#if replaceResult.ok}
							<Alert.Root><Alert.Description>{t('replaceStarted', { slot: replaceResult.slot, id: replaceResult.op })}</Alert.Description></Alert.Root>
							<div class="mt-2"><FollowOperation id={replaceResult.op} /></div>
						{:else if replaceResult.error.code === 'requires_guided_recovery'}
							<Alert.Root>
								<Alert.Title>{t('guidedRecoveryTitle')}</Alert.Title>
								<Alert.Description>{t('guidedRecoveryBody', { slot: replaceResult.slot })}</Alert.Description>
							</Alert.Root>
						{:else}
							<ApiGate error={replaceResult.error} />
						{/if}
					{/if}
				</div>
				<p class="mt-3 text-xs text-muted-foreground">{t('replaceHint')} {t('replacePlanHint')}</p>
			</Card.Content>
		</Card.Root>

		<Card.Root class="lg:col-span-2">
			<Card.Header>
				<Card.Title><h2>{t('outageDiagnosis')}</h2></Card.Title>
				<Card.Description>{t('outageHint')}</Card.Description>
			</Card.Header>
			<Card.Content class="space-y-3">
				{#if diag.status === 'idle'}
					<Button variant="outline" onclick={diagnose}>{t('runDiagnosis')}</Button>
				{:else if diag.status === 'loading'}
					<Loading />
				{:else if diag.status === 'error'}
					<ApiGate error={diag.error} retry={diagnose} />
				{:else}
					{@const d = diag.value}
					<p class="text-sm">
						<span class="text-muted-foreground">{t('candidate')}:</span>
						{#if d.candidate}<span class="font-mono">{d.candidate}</span>{:else}<Status value="unknown" /> ({t('noCandidate')}){/if}
					</p>
					{#if d.positions.length > 0}
						<Table.Root>
							<Table.Header><Table.Row><Table.Head>{t('colId')}</Table.Head><Table.Head>{t('seqno')}</Table.Head><Table.Head>{t('source')}</Table.Head><Table.Head>{t('outageSafe')}</Table.Head></Table.Row></Table.Header>
							<Table.Body>
								{#each d.positions as p (p.slot)}
									<Table.Row>
										<Table.Cell class="font-mono text-xs">{p.slot}</Table.Cell>
										<Table.Cell>{p.seqno ?? t('stateUnknown')}</Table.Cell>
										<Table.Cell>{p.source}{p.empty ? ` (${t('outageEmpty')})` : ''}</Table.Cell>
										<Table.Cell>{p.safe_to_bootstrap === undefined ? t('stateUnknown') : p.safe_to_bootstrap ? t('yes') : t('no')}</Table.Cell>
									</Table.Row>
								{/each}
							</Table.Body>
						</Table.Root>
					{/if}
					{#if d.risks && d.risks.length > 0}
						<div>
							<h3 class="text-sm font-medium">{t('risks')}</h3>
							<ul class="list-disc pl-5 text-sm">{#each d.risks as r, i (i)}<li><span class="font-mono text-xs">{r.kind}{r.slot ? ' / ' + r.slot : ''}</span>: {r.detail}</li>{/each}</ul>
						</div>
					{/if}
					{#if d.reasons.length > 0}
						<ul class="list-disc pl-5 text-sm text-muted-foreground">{#each d.reasons as r, i (i)}<li>{r}</li>{/each}</ul>
					{/if}
					{@const hi = highestSeqno(d)}
					{@const unk = unknownPositions(d)}
					{#if hi !== undefined}<p class="text-sm">{t('outageHighest', { seqno: hi })}</p>{/if}
					{#if unk.length > 0}
						<div class="space-y-1">
							<p class="text-sm">{t('outageUnknownPositions', { slots: unk.join(', ') })}</p>
							<Button variant="outline" size="sm" disabled={recovering} onclick={recoverPositions}>{t('outageRecoverPositions')}</Button>
							<p class="text-xs text-muted-foreground">{t('outageRecoverHint')}</p>
						</div>
					{/if}
					<Alert.Root><Alert.Description>{t('approvalRequired')}</Alert.Description></Alert.Root>
					{#if canRequestRecovery(d) && !outagePlan}
						<div class="space-y-2 rounded-md border p-3">
							<p class="text-xs text-muted-foreground">{t('outageRequestHint')}</p>
							<Button disabled={planning} onclick={() => requestRecovery(d.candidate ?? '')}>{t('outageRequest', { candidate: d.candidate ?? '' })}</Button>
						</div>
					{/if}
					{#if outagePlan?.status === 'error'}
						<ApiGate error={outagePlan.error} />
					{:else if outagePlan}
						<Alert.Root><Alert.Description>{t('outagePlanned', { id: outagePlan.value.id, candidate: outagePlan.value.candidate })}</Alert.Description></Alert.Root>
						<FollowOperation id={outagePlan.value.id} />
					{/if}
					<Button variant="ghost" size="sm" onclick={diagnose}>{t('refresh')}</Button>
				{/if}
			</Card.Content>
		</Card.Root>

		<DriftPanel cluster={c.id} />

		<ClusterOperationPanel cluster={c.id} nodes={c.nodes} />

		<ProxyWriterPanel cluster={c.id} nodes={c.nodes} />

		<Card.Root class="lg:col-span-2">
			<Card.Header>
				<Card.Title><h2>{t('clusterEvents')}</h2></Card.Title>
				<Card.Description>{t('clusterEventsHint')}</Card.Description>
			</Card.Header>
			<Card.Content>
				{#if events.state.status === 'loading'}
					<Loading />
				{:else if events.state.status === 'error'}
					<ApiGate error={events.state.error} retry={events.reload} />
				{:else}
					{@const list = eventsForCluster(events.state.value, c.id)}
					{#if list.length === 0}
						<p class="text-sm text-muted-foreground">{t('clusterEventsEmpty')}</p>
					{:else}
						<ul class="divide-y text-sm">
							{#each list as e (e.id)}
								<li class="flex flex-wrap items-baseline gap-x-3 py-2">
									<Status kind="severity" value={e.severity} />
									<time class="text-xs text-muted-foreground" datetime={e.time} title={dateTime(e.time, locale.value)}>{relativeAge(e.time, locale.value)}</time>
									<span class="min-w-0 flex-1">{e.message}</span>
								</li>
							{/each}
						</ul>
					{/if}
				{/if}
				<p class="mt-2 text-xs"><a class="underline underline-offset-4" href="/events">{t('allEvents')}</a></p>
			</Card.Content>
		</Card.Root>

		<Card.Root class="border-red-300 lg:col-span-2 dark:border-red-900">
			<Card.Header>
				<Card.Title><h2>{t('deleteZone')}</h2></Card.Title>
				<Card.Description>{t('deleteIntro')}</Card.Description>
			</Card.Header>
			<Card.Content class="space-y-3">
				{#if deletion?.status === 'ok'}
					<Alert.Root><Alert.Description>{t('deletePlanned', { id: deletion.id })}</Alert.Description></Alert.Root>
					<FollowOperation id={deletion.id} />
				{:else}
					<div class="max-w-sm space-y-1.5">
						<Label for="del-name">{t('deleteTypeLabel')}</Label>
						<Input id="del-name" bind:value={typed} autocomplete="off" aria-describedby="del-hint" />
						<p id="del-hint" class="text-xs text-muted-foreground">{t('deleteTypeName', { name: c.name })}</p>
					</div>
					<Button variant="destructive" disabled={deleting || !deleteConfirmed(typed, c.name)} onclick={planDelete}>{t('deletePlan')}</Button>
					<div aria-live="polite">{#if deletion?.status === 'error'}<ApiGate error={deletion.error} />{/if}</div>
				{/if}
			</Card.Content>
		</Card.Root>
	</div>
{/if}
