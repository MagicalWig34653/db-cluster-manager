<script lang="ts">
	import { api, asApiError, type ApiError, type PatchHistoryEntry, type PatchHostView } from '../../../lib/api';
	import { hostAttention, sortPatchHosts } from '../../../lib/patch';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime } from '../../../lib/format';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';
	import PatchGroups from '../../../lib/components/app/PatchGroups.svelte';
	import PatchPlans from '../../../lib/components/app/PatchPlans.svelte';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';

	const hosts = load(api.patchHosts);
	const rows = $derived(hosts.state.status === 'ok' ? sortPatchHosts(hosts.state.value.items) : []);
	let open = $state('');
	let view = $state<PatchHostView | undefined>();
	let history = $state<PatchHistoryEntry[]>([]);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();

	const tri = (v: string) => (v === 'yes' ? t('patchYes') : v === 'no' ? t('patchNo') : t('patchUnknown'));

	async function show(id: string) {
		if (open === id) { open = ''; return; }
		open = id; view = undefined; history = []; error = undefined; busy = true;
		try { [view, history] = await Promise.all([api.patchHost(id), api.patchHistory(id)]); }
		catch (e) { error = asApiError(e); }
		finally { busy = false; }
	}
	async function poll(id: string) {
		busy = true; error = undefined;
		try { view = await api.pollPatchHost(id); history = await api.patchHistory(id); hosts.reload(); }
		catch (e) { error = asApiError(e); }
		finally { busy = false; }
	}
</script>

<PageHeader title={t('patchTitle')} intro={t('patchIntro')} />
{#if hosts.state.status === 'loading'}<Loading />
{:else if hosts.state.status === 'error'}<ApiGate error={hosts.state.error} retry={hosts.reload} />
{:else if !rows.length}<p class="text-sm text-muted-foreground">{t('patchNone')}</p>
{:else}
	<div class="overflow-x-auto">
	<Table.Root>
		<Table.Header><Table.Row>
			<Table.Head>{t('patchHost')}</Table.Head><Table.Head>{t('patchGroup')}</Table.Head><Table.Head>{t('patchUpdates')}</Table.Head>
			<Table.Head>{t('patchSecurity')}</Table.Head><Table.Head>{t('patchReboot')}</Table.Head><Table.Head>{t('patchReported')}</Table.Head><Table.Head></Table.Head>
		</Table.Row></Table.Header>
		<Table.Body>
			{#each rows as h (h.host.id)}
				{@const attention = hostAttention(h)}
				<Table.Row>
					<Table.Cell><div class="font-medium">{h.host.name}</div>
						<div class="text-xs {attention === 'ok' ? 'text-muted-foreground' : attention === 'error' || attention === 'security' || attention === 'stale' ? 'text-destructive' : 'text-amber-700 dark:text-amber-400'}">{t(`patchAttention_${attention}`)}{#if h.host.lastPollError}: {h.host.lastPollError}{/if}</div></Table.Cell>
					<Table.Cell>{h.host.group ?? '—'}</Table.Cell>
					<Table.Cell>{h.updatesPending}</Table.Cell>
					<Table.Cell>{h.securityKnown ? h.securityPending : t('patchUnknown')}</Table.Cell>
					<Table.Cell>{tri(h.rebootRequired)}</Table.Cell>
					<Table.Cell>{dateTime(h.collectedAt, locale.value) ?? '—'}</Table.Cell>
					<Table.Cell><Button variant="outline" size="sm" aria-expanded={open === h.host.id} onclick={() => show(h.host.id)}>{t('patchDetails')}</Button></Table.Cell>
				</Table.Row>
				{#if open === h.host.id}
					<Table.Row><Table.Cell colspan={7}>
						{#if busy && !view}<Loading />{/if}
						{#if error}<ApiGate {error} retry={() => show(h.host.id)} />{/if}
						{#if view}
							<div class="space-y-3 py-2">
								<Button size="sm" disabled={busy} onclick={() => poll(h.host.id)}>{busy ? t('patchPolling') : t('patchPoll')}</Button>
								{#each view.warnings as w (w)}<p class="text-sm text-amber-700 dark:text-amber-400">{w}</p>{/each}
								{#if !view.summary.securityKnown}<p class="text-sm text-amber-700 dark:text-amber-400">{t('patchSecurityUnknown')}</p>{/if}
								{#if view.rebootReasons.length}<p class="text-sm">{t('patchRebootReasons')}: {view.rebootReasons.join(', ')}</p>{/if}
								{#if view.updates.length}
									<Table.Root><Table.Header><Table.Row><Table.Head>{t('patchPackage')}</Table.Head><Table.Head>{t('patchInstalled')}</Table.Head><Table.Head>{t('patchAvailable')}</Table.Head><Table.Head>{t('patchSecurity')}</Table.Head></Table.Row></Table.Header>
									<Table.Body>{#each view.updates as u (u.name + (u.arch ?? ''))}
										<Table.Row><Table.Cell>{u.name}{#if u.class === 'container-runtime'}<div class="text-xs text-amber-700 dark:text-amber-400">{t('patchRuntimeClass')}</div>{/if}</Table.Cell>
										<Table.Cell class="font-mono text-xs">{u.installed ?? '—'}</Table.Cell><Table.Cell class="font-mono text-xs">{u.available}</Table.Cell>
										<Table.Cell>{u.security ? (u.advisories?.join(', ') || t('patchYes')) : '—'}</Table.Cell></Table.Row>
									{/each}</Table.Body></Table.Root>
								{:else}<p class="text-sm text-muted-foreground">{t('patchNoUpdates')}</p>{/if}
								<h2 class="font-medium">{t('patchHistory')}</h2>
								{#if history.length}<ul class="space-y-1 text-sm">{#each history as e (e.at + e.kind)}<li>{dateTime(e.at, locale.value)} · {e.kind}{#if e.result} · {e.result}{/if}{#if e.updatesPending !== undefined} · {e.updatesPending}/{e.securityPending ?? 0}{/if}</li>{/each}</ul>
								{:else}<p class="text-sm text-muted-foreground">{t('patchNoHistory')}</p>{/if}
							</div>
						{/if}
					</Table.Cell></Table.Row>
				{/if}
			{/each}
		</Table.Body>
	</Table.Root>
	</div>
{/if}
{#if hosts.state.status === 'ok'}<PatchPlans hosts={hosts.state.value.items} /><PatchGroups hosts={hosts.state.value.items} onchange={hosts.refresh} />{/if}
