<script lang="ts">
	import { api, asApiError, type ApiError, type HostSource } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime } from '../../../lib/format';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';

	const sources = load(api.hostSources);
	let busy = $state('');
	let started = $state<{ source: string; operation: string } | undefined>();
	let error = $state<ApiError | undefined>();

	async function start(s: HostSource) {
		busy = s.id; error = undefined; started = undefined;
		try { started = { source: s.name, operation: (await api.startHostBackup(s.id)).id }; sources.refresh(); }
		catch (e) { error = asApiError(e); }
		finally { busy = ''; }
	}
	// The repository can carry credentials in its URL; show scheme and host only.
	const repo = (r: string) => r.replace(/\/\/[^@/]*@/, '//');
</script>

<PageHeader title={t('hostBackupsTitle')} intro={t('hostBackupsIntro')} />
{#if started}<p class="text-sm" role="status">{t('hostBackupsStarted')}: {started.source} · <a class="underline" href="/operations">{started.operation}</a></p>{/if}
{#if error}<p class="text-sm text-destructive" role="alert">{error.message}</p>{/if}
{#if sources.state.status === 'loading'}<Loading />
{:else if sources.state.status === 'error'}<ApiGate error={sources.state.error} retry={sources.reload} />
{:else if !sources.state.value.items.length}<p class="text-sm text-muted-foreground">{t('hostBackupsNone')}</p>
{:else}
	<div class="overflow-x-auto">
	<Table.Root>
		<Table.Header><Table.Row>
			<Table.Head>{t('hostBackupsSource')}</Table.Head><Table.Head>{t('hostBackupsPaths')}</Table.Head><Table.Head>{t('hostBackupsTarget')}</Table.Head>
			<Table.Head>{t('hostBackupsSchedule')}</Table.Head><Table.Head>{t('hostBackupsLastRun')}</Table.Head><Table.Head></Table.Head>
		</Table.Row></Table.Header>
		<Table.Body>
			{#each sources.state.value.items as s (s.id)}
				<Table.Row>
					<Table.Cell><div class="font-medium">{s.name}</div>
						<div class="text-xs text-muted-foreground">{s.transport === 'ssh-plan' ? t('hostBackupsExternal') : t(`hostBackupsRuntime_${s.runtime ?? 'none'}`)}</div>
						{#if s.runtimeNote}<div class="text-xs text-amber-700 dark:text-amber-400">{s.runtimeNote}</div>{/if}</Table.Cell>
					<Table.Cell class="font-mono text-xs">{s.paths.join(', ')}</Table.Cell>
					<Table.Cell><div>{s.backend}</div><div class="font-mono text-xs text-muted-foreground">{repo(s.repository)}</div></Table.Cell>
					<Table.Cell class="font-mono text-xs">{s.schedule ?? t('hostBackupsManual')}</Table.Cell>
					<Table.Cell>{#if s.lastRunAt}{dateTime(s.lastRunAt, locale.value)}<div class="text-xs {s.lastRunStatus === 'failed' ? 'text-destructive' : 'text-muted-foreground'}">{s.lastRunStatus}</div>{:else}—{/if}</Table.Cell>
					<Table.Cell>{#if s.transport === 'agent'}<Button size="sm" disabled={busy !== '' || !s.runtime} onclick={() => start(s)}>{t('hostBackupsStart')}</Button>{/if}</Table.Cell>
				</Table.Row>
			{/each}
		</Table.Body>
	</Table.Root>
	</div>
{/if}
