<script lang="ts">
	import { api } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const hosts = load(api.hosts);
	const enrollLabel: Record<string, MessageKey> = {
		discovered: 'enrollDiscovered',
		enrolled: 'enrollEnrolled',
		revoked: 'enrollRevoked'
	};
	// Swarm node status maps to the shared state vocabulary; anything else is unknown.
	const nodeState = (s?: string) => (s === 'ready' ? 'healthy' : s === 'down' ? 'unavailable' : 'unknown');
</script>

<PageHeader title={t('navHosts')} intro={t('hostsIntro')} />

{#if hosts.state.status === 'loading'}
	<Loading />
{:else if hosts.state.status === 'error'}
	<ApiGate error={hosts.state.error} retry={hosts.reload} />
{:else if hosts.state.value.length === 0}
	<p class="text-sm text-muted-foreground">{t('hostsEmpty')}</p>
{:else}
	<div class="rounded-lg border">
		<Table.Root>
			<Table.Header>
				<Table.Row>
					<Table.Head>{t('colHost')}</Table.Head>
					<Table.Head>{t('colSwarm')}</Table.Head>
					<Table.Head>{t('colEnrollment')}</Table.Head>
					<Table.Head>{t('colState')}</Table.Head>
					<Table.Head>{t('colAgentVersion')}</Table.Head>
					<Table.Head>{t('colLastSeen')}</Table.Head>
				</Table.Row>
			</Table.Header>
			<Table.Body>
				{#each hosts.state.value as h (h.swarm + '/' + h.nodeId)}
					<Table.Row>
						<Table.Cell class="font-medium">
							{h.hostname ?? h.nodeId}
							{#if h.spare}<span class="ml-1 text-xs text-muted-foreground">({t('spare')})</span>{/if}
						</Table.Cell>
						<Table.Cell class="font-mono text-xs">{h.swarm}</Table.Cell>
						<Table.Cell>{t(enrollLabel[h.enrollment])}</Table.Cell>
						<Table.Cell><Status value={nodeState(h.state)} /></Table.Cell>
						<Table.Cell>{h.agentVersion ?? t('stateUnknown')}</Table.Cell>
						<Table.Cell>
							{#if h.lastSeen}<Freshness at={h.lastSeen} />{:else}<span class="text-muted-foreground">{t('stateUnknown')}</span>{/if}
						</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
	<p class="mt-3 text-xs text-muted-foreground" data-locale={locale.value}>{t('hostsHint')}</p>
{/if}
