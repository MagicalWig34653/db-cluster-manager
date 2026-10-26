<script lang="ts">
	import StateView from '../../../lib/components/app/StateView.svelte';

	import { api, asApiError, type ApiError } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime, newestFirst } from '../../../lib/format';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	// Assumed: GET /notifications, POST /notifications/{id}/read|ack,
	// GET/PUT /settings/notifications (docs/DESIGN.md).
	const inbox = load(api.notifications);
	let actionError = $state<ApiError | undefined>();
	async function mark(id: string, action: 'read' | 'ack') {
		actionError = undefined;
		try {
			await api.markNotification(id, action);
			inbox.reload();
		} catch (e) {
			actionError = asApiError(e);
		}
	}

</script>

<PageHeader title={t('navNotifications')} intro={t('notificationsIntro')} />

<p class="mb-4 text-sm"><a class="underline underline-offset-4" href="/notifications/settings">{t('notifySettingsTitle')}</a></p>

<div class="grid gap-4">
	<Card.Root>
		<Card.Header><Card.Title><h2>{t('inbox')}</h2></Card.Title></Card.Header>
		<Card.Content>
			{#if inbox.state.status === 'loading'}
				<Loading />
			{:else if inbox.state.status === 'error'}
				<ApiGate error={inbox.state.error} retry={inbox.reload} />
			{:else if inbox.state.value.length === 0}
				<StateView kind="empty" message="inboxEmpty" />
			{:else}
				<ul class="divide-y">
					{#each newestFirst(inbox.state.value, (n) => n.time) as n (n.id)}
						<li class="flex flex-wrap items-start gap-3 py-3">
							<div class="min-w-0 flex-1 space-y-1">
								<p class="flex flex-wrap items-center gap-x-3 text-sm {n.read ? 'text-muted-foreground' : 'font-medium'}">
									<Status kind="severity" value={n.severity} />
									{#if !n.read}<span class="rounded bg-primary px-1.5 text-xs text-primary-foreground">{t('unread')}</span>{/if}
								</p>
								<p class="text-sm">{n.message}</p>
								<p class="text-xs text-muted-foreground">{dateTime(n.time, locale.value)}{n.cluster ? ` · ${n.cluster}` : ''}{n.acknowledged ? ` · ${t('acknowledged')}` : ''}</p>
							</div>
							<div class="flex gap-1">
								{#if !n.read}<Button size="sm" variant="outline" onclick={() => mark(n.id, 'read')}>{t('markRead')}</Button>{/if}
								{#if !n.acknowledged}<Button size="sm" variant="outline" onclick={() => mark(n.id, 'ack')}>{t('acknowledge')}</Button>{/if}
							</div>
						</li>
					{/each}
				</ul>
			{/if}
			<div aria-live="polite">{#if actionError}<ApiGate error={actionError} />{/if}</div>
		</Card.Content>
	</Card.Root>

</div>
