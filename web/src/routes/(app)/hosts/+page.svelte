<script lang="ts">
	import { api, asApiError, type ApiError, type Host } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import { canEnroll, canRevoke } from '../../../lib/operations';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const hosts = load(api.hosts);
	const enrollLabel: Record<string, MessageKey> = {
		discovered: 'enrollDiscovered',
		enrolled: 'enrollEnrolled',
		revoked: 'enrollRevoked'
	};
	// Swarm node status maps to the shared state vocabulary; anything else is unknown.
	const nodeState = (s?: string) => (s === 'ready' ? 'healthy' : s === 'down' ? 'unavailable' : 'unknown');

	let busy = $state<string | undefined>(); // nodeId of the running action
	let revoking = $state<string | undefined>(); // nodeId awaiting confirmation
	let error = $state<{ node: string; error: ApiError } | undefined>();
	async function act(h: Host, action: 'enroll' | 'revoke') {
		busy = h.nodeId;
		error = undefined;
		try {
			await api.setEnrollment(h.nodeId, action);
			hosts.refresh();
		} catch (e) {
			error = { node: h.nodeId, error: asApiError(e) };
		} finally {
			busy = undefined;
			revoking = undefined;
		}
	}
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
					<Table.Head>{t('colAgentIdentity')}</Table.Head>
					<Table.Head>{t('colActions')}</Table.Head>
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
						<Table.Cell>{h.agentIdentity ? t('identityRecorded') : t('identityNone')}</Table.Cell>
						<Table.Cell>
							<div class="flex flex-wrap gap-2">
								{#if canEnroll(h)}
									<Button size="sm" variant="outline" disabled={busy !== undefined} onclick={() => act(h, 'enroll')}>{t('enrollAction')}</Button>
								{/if}
								{#if canRevoke(h) && revoking !== h.nodeId}
									<Button size="sm" variant="outline" disabled={busy !== undefined} onclick={() => (revoking = h.nodeId)}>{t('revokeAction')}</Button>
								{/if}
							</div>
							{#if revoking === h.nodeId}
								<Alert.Root variant="destructive" class="mt-2">
									<Alert.Description class="space-y-2">
										<p>{t('revokeConfirm', { host: h.hostname ?? h.nodeId })}</p>
										<div class="flex gap-2">
											<Button size="sm" variant="destructive" disabled={busy !== undefined} onclick={() => act(h, 'revoke')}>{t('revokeYes')}</Button>
											<Button size="sm" variant="ghost" onclick={() => (revoking = undefined)}>{t('cancel')}</Button>
										</div>
									</Alert.Description>
								</Alert.Root>
							{/if}
							{#if error?.node === h.nodeId}<div class="mt-2" aria-live="polite"><ErrorNotice error={error.error} /></div>{/if}
						</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
	</div>
	<p class="mt-3 text-xs text-muted-foreground">{t('hostsHint')}</p>
{/if}
