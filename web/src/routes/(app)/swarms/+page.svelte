<script lang="ts">
	import StateView from '../../../lib/components/app/StateView.svelte';

	import { api, asApiError, type AgentDeployment, type ApiError, type Swarm } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import { deploymentFailed, validateSwarm } from '../../../lib/operations';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const swarms = load(api.swarms);

	// Connect form: POST /swarms.
	let form = $state({ name: '', dockerHost: '' });
	let shown = $state(false);
	let connecting = $state(false);
	let connectError = $state<ApiError | undefined>();
	let connected = $state<string | undefined>();
	const invalid = $derived(validateSwarm(form));
	async function connect(e: SubmitEvent) {
		e.preventDefault();
		shown = true;
		if (invalid.length > 0) return;
		connecting = true;
		connectError = undefined;
		connected = undefined;
		try {
			const s = await api.connectSwarm({ name: form.name.trim(), dockerHost: form.dockerHost.trim() });
			connected = s.name;
			form = { name: '', dockerHost: '' };
			shown = false;
			swarms.refresh();
		} catch (err) {
			connectError = asApiError(err);
		} finally {
			connecting = false;
		}
	}

	// Per-Swarm actions: sync (204) and agent deployment (200 with a report).
	type Result =
		| { swarm: string; kind: 'sync' }
		| { swarm: string; kind: 'deploy'; report: AgentDeployment }
		| { swarm: string; kind: 'error'; error: ApiError };
	let busy = $state<string | undefined>();
	let result = $state<Result | undefined>();
	async function sync(s: Swarm) {
		busy = s.id;
		result = undefined;
		try {
			await api.syncSwarm(s.id);
			result = { swarm: s.id, kind: 'sync' };
			swarms.refresh();
		} catch (e) {
			result = { swarm: s.id, kind: 'error', error: asApiError(e) };
		} finally {
			busy = undefined;
		}
	}
	async function deploy(s: Swarm) {
		busy = s.id;
		result = undefined;
		try {
			result = { swarm: s.id, kind: 'deploy', report: await api.deployAgents(s.id) };
		} catch (e) {
			result = { swarm: s.id, kind: 'error', error: asApiError(e) };
		} finally {
			busy = undefined;
		}
	}
	const nameOf = (id: string) => (swarms.state.status === 'ok' ? swarms.state.value.find((s) => s.id === id)?.name : undefined) ?? id;
	const fieldKey: Record<'name' | 'dockerHost', MessageKey> = { name: 'valSwarmName', dockerHost: 'valDockerHost' };
</script>

<PageHeader title={t('swarmsTitle')} intro={t('swarmsIntro')} />

<div class="space-y-6">
	{#if swarms.state.status === 'loading'}
		<Loading />
	{:else if swarms.state.status === 'error'}
		<ErrorNotice error={swarms.state.error} retry={swarms.reload} />
	{:else if swarms.state.value.length === 0}
		<StateView kind="empty" message="swarmsEmpty" />
	{:else}
		<div class="rounded-lg border">
			<Table.Root>
				<Table.Header>
					<Table.Row>
						<Table.Head>{t('colName')}</Table.Head>
						<Table.Head>{t('colId')}</Table.Head>
						<Table.Head>{t('colState')}</Table.Head>
						<Table.Head>{t('colFreshness')}</Table.Head>
						<Table.Head>{t('colActions')}</Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each swarms.state.value as s (s.id)}
						<Table.Row>
							<Table.Cell class="font-medium">{s.name}</Table.Cell>
							<Table.Cell class="font-mono text-xs">{s.id}</Table.Cell>
							<Table.Cell><Status value={s.state} /></Table.Cell>
							<Table.Cell><Freshness at={s.observedAt} /></Table.Cell>
							<Table.Cell>
								<div class="flex flex-wrap gap-2">
									<Button size="sm" variant="outline" disabled={busy !== undefined} onclick={() => sync(s)}>{t('syncSwarm')}</Button>
									<Button size="sm" variant="outline" disabled={busy !== undefined} onclick={() => deploy(s)}>{t('deployAgents')}</Button>
								</div>
							</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
	{/if}

	<div aria-live="polite">
		{#if result?.kind === 'error'}
			<ErrorNotice error={result.error} />
		{:else if result?.kind === 'sync'}
			<Alert.Root><Alert.Description>{t('syncStarted', { name: nameOf(result.swarm) })}</Alert.Description></Alert.Root>
		{:else if result?.kind === 'deploy'}
			{@const r = result.report}
			<Alert.Root variant={deploymentFailed(r) ? 'destructive' : 'default'}>
				<Alert.Description class="space-y-1">
					<p>
						{t('deployDone', {
							name: nameOf(result.swarm),
							service: r.serviceId ? `${r.serviceId} (${r.created ? t('deployServiceCreated') : t('deployServiceExisting')})` : '—',
							labeled: r.labeled.length,
							unlabeled: r.unlabeled.length,
							failed: r.failed.length
						})}
					</p>
					{#if deploymentFailed(r)}<p>{t('deployFailedOn', { nodes: r.failed.join(', ') })}</p>{/if}
				</Alert.Description>
			</Alert.Root>
		{/if}
	</div>

	<Card.Root>
		<Card.Header>
			<Card.Title><h2>{t('swarmConnectTitle')}</h2></Card.Title>
			<Card.Description>{t('swarmConnectIntro')}</Card.Description>
		</Card.Header>
		<Card.Content>
			<form class="space-y-4" onsubmit={connect}>
				{#each [{ id: 'sw-name', key: 'name', label: 'swarmName', hint: undefined }, { id: 'sw-host', key: 'dockerHost', label: 'dockerHost', hint: 'dockerHostHint' }] as const as f (f.id)}
					{@const bad = shown && invalid.includes(f.key)}
					<div class="space-y-1.5">
						<Label for={f.id}>{t(f.label)}</Label>
						<Input id={f.id} bind:value={form[f.key]} autocomplete="off" class={f.key === 'dockerHost' ? 'font-mono' : ''} aria-invalid={bad ? true : undefined} aria-describedby="{f.id}-d" />
						<p id="{f.id}-d" class="text-xs {bad ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{bad ? t(fieldKey[f.key]) : f.hint ? t(f.hint) : ''}</p>
					</div>
				{/each}
				<Button type="submit" disabled={connecting}>{t('connectSwarm')}</Button>
				<div aria-live="polite">
					{#if connectError}<ErrorNotice error={connectError} />{/if}
					{#if connected}<p class="text-sm">{t('swarmConnected', { name: connected })}</p>{/if}
				</div>
			</form>
		</Card.Content>
	</Card.Root>
</div>
