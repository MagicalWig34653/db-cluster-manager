<script lang="ts">
	import { api, asApiError, type ApiError, type NotificationSettings } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime, newestFirst } from '../../../lib/format';
	import { checkClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	// Assumed: GET /notifications, POST /notifications/{id}/read|ack,
	// GET/PUT /settings/notifications (docs/DESIGN.md).
	const inbox = load(api.notifications);
	const settings = load(api.notificationSettings);
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

	let url = $state('');
	let secret = $state('');
	let webhookOn = $state(false);
	let emailOn = $state(false);
	let recipients = $state('');
	let loadedFrom: NotificationSettings | undefined;
	let saving = $state(false);
	let saved = $state(false);
	let saveError = $state<ApiError | undefined>();
	$effect(() => {
		if (settings.state.status === 'ok' && settings.state.value !== loadedFrom) {
			const s = (loadedFrom = settings.state.value);
			url = s.webhook.url ?? '';
			webhookOn = s.webhook.enabled;
			emailOn = s.email.enabled;
			recipients = s.email.recipients.join(', ');
			secret = '';
		}
	});
	async function save(e: SubmitEvent) {
		e.preventDefault();
		saving = true;
		saved = false;
		saveError = undefined;
		try {
			await api.saveNotificationSettings({
				webhook: { enabled: webhookOn, url: url.trim(), ...(secret ? { secret } : {}) },
				email: { enabled: emailOn, recipients: recipients.split(',').map((r) => r.trim()).filter(Boolean) }
			});
			saved = true;
			secret = '';
			settings.reload();
		} catch (err) {
			saveError = asApiError(err);
		} finally {
			saving = false;
		}
	}
</script>

<PageHeader title={t('navNotifications')} intro={t('notificationsIntro')} />

<div class="grid gap-4 lg:grid-cols-2">
	<Card.Root>
		<Card.Header><Card.Title><h2>{t('inbox')}</h2></Card.Title></Card.Header>
		<Card.Content>
			{#if inbox.state.status === 'loading'}
				<Loading />
			{:else if inbox.state.status === 'error'}
				<ApiGate error={inbox.state.error} retry={inbox.reload} />
			{:else if inbox.state.value.length === 0}
				<p class="text-sm text-muted-foreground">{t('inboxEmpty')}</p>
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

	<Card.Root>
		<Card.Header><Card.Title><h2>{t('channels')}</h2></Card.Title></Card.Header>
		<Card.Content>
			{#if settings.state.status === 'loading'}
				<Loading />
			{:else if settings.state.status === 'error'}
				<ApiGate error={settings.state.error} retry={settings.reload} />
			{:else}
				<form class="space-y-4" onsubmit={save}>
					<fieldset class="space-y-3">
						<legend class="text-sm font-medium">{t('webhook')}</legend>
						<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={webhookOn} />{t('enabled')}</label>
						<div class="space-y-1.5">
							<Label for="wh-url">URL</Label>
							<Input id="wh-url" type="url" bind:value={url} autocomplete="off" placeholder="https://" />
						</div>
						<div class="space-y-1.5">
							<Label for="wh-secret">{t('webhookSecret')}</Label>
							<Input id="wh-secret" type="password" bind:value={secret} autocomplete="new-password" aria-describedby="wh-secret-hint" />
							<p id="wh-secret-hint" class="text-xs text-muted-foreground">
								{settings.state.value.webhook.secretSet ? t('secretSet') : t('secretNotSet')} {t('secretHint')}
							</p>
						</div>
					</fieldset>
					<fieldset class="space-y-3">
						<legend class="text-sm font-medium">{t('email')}</legend>
						<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={emailOn} />{t('enabled')}</label>
						<div class="space-y-1.5">
							<Label for="em-rcpt">{t('recipients')}</Label>
							<Input id="em-rcpt" bind:value={recipients} autocomplete="off" aria-describedby="em-hint" />
							<p id="em-hint" class="text-xs text-muted-foreground">{t('recipientsHint')}</p>
						</div>
					</fieldset>
					<Button type="submit" disabled={saving}>{t('save')}</Button>
					<div aria-live="polite">
						{#if saveError}<ApiGate error={saveError} />{/if}
						{#if saved}<Alert.Root><Alert.Description>{t('saved')}</Alert.Description></Alert.Root>{/if}
					</div>
				</form>
			{/if}
		</Card.Content>
	</Card.Root>
</div>
