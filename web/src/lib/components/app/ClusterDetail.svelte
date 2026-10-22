<script lang="ts">
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import { api, asApiError, type ApiError, type Diagnosis } from '../../api';
	import { load } from '../../load.svelte';
	import { t } from '../../locale.svelte';
	import * as Card from '../ui/card/index.js';
	import * as Table from '../ui/table/index.js';
	import PageHeader from './PageHeader.svelte';
	import Status from './Status.svelte';
	import Freshness from './Freshness.svelte';
	import ErrorNotice from './ErrorNotice.svelte';
	import Loading from './Loading.svelte';
	import ApiGate from './ApiGate.svelte';
	import { Button } from '../ui/button/index.js';
	import * as Alert from '../ui/alert/index.js';

	let { id }: { id: string } = $props();
	// The parent re-creates this component when the id changes.
	// svelte-ignore state_referenced_locally
	const cluster = load(() => api.cluster(id));

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
				<p class="mt-3 text-xs text-muted-foreground">{t('replaceHint')}</p>
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
							<Table.Header><Table.Row><Table.Head>{t('colId')}</Table.Head><Table.Head>seqno</Table.Head><Table.Head>{t('source')}</Table.Head></Table.Row></Table.Header>
							<Table.Body>
								{#each d.positions as p (p.slot)}
									<Table.Row>
										<Table.Cell class="font-mono text-xs">{p.slot}</Table.Cell>
										<Table.Cell>{p.seqno ?? t('stateUnknown')}</Table.Cell>
										<Table.Cell>{p.source}</Table.Cell>
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
					<Alert.Root><Alert.Description>{t('approvalRequired')}</Alert.Description></Alert.Root>
					<Button variant="ghost" size="sm" onclick={diagnose}>{t('refresh')}</Button>
				{/if}
			</Card.Content>
		</Card.Root>
	</div>
{/if}
