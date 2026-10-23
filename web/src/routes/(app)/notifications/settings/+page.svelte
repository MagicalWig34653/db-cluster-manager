<script lang="ts">
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import { api, asApiError, minSeverities, type ApiError, type NotificationChannel, type NotificationSettings } from '../../../../lib/api';
	import { buildNotify, isMinSeverity, notifyFormOf, validateNotify, type NotifyError, type NotifyForm } from '../../../../lib/lifecycle';
	import { load } from '../../../../lib/load.svelte';
	import { t } from '../../../../lib/locale.svelte';
	import type { MessageKey } from '../../../../lib/i18n';
	import { checkClass, selectClass } from '../../../../lib/components/app/fieldClass';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';

	// GET/PUT /settings/notifications and POST /settings/notifications/test.
	// The webhook secret and the SMTP password are write-only: they are sent
	// only when typed and are never shown.
	const settings = load(api.notificationSettings);
	let form = $state<NotifyForm | undefined>();
	let loadedFrom: NotificationSettings | undefined;
	let invalid = $state<NotifyError[]>([]);
	let saving = $state(false);
	let saved = $state(false);
	let saveError = $state<ApiError | undefined>();
	let testing = $state<NotificationChannel | undefined>();
	let tested = $state<{ channel: NotificationChannel; delivered: boolean } | undefined>();
	let testError = $state<ApiError | undefined>();
	$effect(() => {
		if (settings.state.status === 'ok' && settings.state.value !== loadedFrom) {
			loadedFrom = settings.state.value;
			form = notifyFormOf(settings.state.value);
		}
	});
	const errKey: Record<NotifyError, MessageKey> = { webhookUrl: 'valWebhookUrl', recipients: 'valRecipients', addr: 'valSmtpAddr', from: 'valFrom' };
	const sevKey: Record<string, MessageKey> = { info: 'sevInfo', warning: 'sevWarning', error: 'sevError', critical: 'sevCritical' };

	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (!form) return;
		invalid = validateNotify(form);
		if (invalid.length > 0) return;
		saving = true;
		saved = false;
		saveError = undefined;
		try {
			await api.saveNotificationSettings(buildNotify(form));
			saved = true;
			settings.reload();
		} catch (err) {
			saveError = asApiError(err);
		} finally {
			saving = false;
		}
	}
	async function test(channel: NotificationChannel) {
		testing = channel;
		tested = undefined;
		testError = undefined;
		try {
			tested = { channel, delivered: (await api.testNotification(channel)).delivered };
		} catch (err) {
			testError = asApiError(err);
		} finally {
			testing = undefined;
		}
	}
</script>

<a href="/notifications" class="mb-3 inline-flex items-center gap-1 text-sm text-muted-foreground underline-offset-4 hover:underline">
	<ArrowLeft class="size-4" aria-hidden="true" />{t('backToNotifications')}
</a>
<PageHeader title={t('notifySettingsTitle')} intro={t('notifySettingsIntro')} />

<Card.Root class="max-w-2xl">
	<Card.Header><Card.Title><h2>{t('channelSettings')}</h2></Card.Title></Card.Header>
	<Card.Content>
		{#if settings.state.status === 'loading'}
			<Loading />
		{:else if settings.state.status === 'error'}
			<ApiGate error={settings.state.error} retry={settings.reload} />
		{:else if form}
			{@const s = settings.state.value}
			<form class="space-y-5" onsubmit={save}>
				<div class="space-y-1.5">
					<Label for="ns-sev">{t('notifyMinSeverity')}</Label>
					<select id="ns-sev" class={selectClass} value={form.minSeverity} onchange={(e) => { if (form && isMinSeverity(e.currentTarget.value)) form.minSeverity = e.currentTarget.value; }} aria-describedby="ns-sev-hint">
						{#each minSeverities as m (m)}<option value={m}>{t(sevKey[m])}</option>{/each}
					</select>
					<p id="ns-sev-hint" class="text-xs text-muted-foreground">{t('notifyMinSeverityHint')}</p>
				</div>

				<fieldset class="space-y-3">
					<legend class="text-sm font-medium">{t('webhook')}</legend>
					<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={form.webhookEnabled} />{t('enabled')}</label>
					<div class="space-y-1.5">
						<Label for="wh-url">URL</Label>
						<Input id="wh-url" type="url" bind:value={form.webhookUrl} autocomplete="off" placeholder="https://" aria-invalid={invalid.includes('webhookUrl')} />
						{#if invalid.includes('webhookUrl')}<p class="text-xs text-red-700 dark:text-red-400">{t(errKey.webhookUrl)}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="wh-secret">{t('webhookSecret')}</Label>
						<Input id="wh-secret" type="password" bind:value={form.webhookSecret} autocomplete="new-password" aria-describedby="wh-secret-hint" />
						<p id="wh-secret-hint" class="text-xs text-muted-foreground">{s.webhook.secretSet ? t('secretSet') : t('secretNotSet')} {t('secretHint')}</p>
					</div>
					<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={form.allowPrivate} aria-describedby="wh-priv-hint" />{t('webhookAllowPrivate')}</label>
					<p id="wh-priv-hint" class="text-xs text-muted-foreground">{t('webhookAllowPrivateHint')}</p>
					<Button type="button" size="sm" variant="outline" disabled={testing !== undefined} onclick={() => test('webhook')}>{t('testSend')}</Button>
				</fieldset>

				<fieldset class="space-y-3">
					<legend class="text-sm font-medium">{t('email')}</legend>
					<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={form.emailEnabled} />{t('enabled')}</label>
					<div class="space-y-1.5">
						<Label for="em-rcpt">{t('recipients')}</Label>
						<Input id="em-rcpt" bind:value={form.recipients} autocomplete="off" aria-describedby="em-hint" aria-invalid={invalid.includes('recipients')} />
						<p id="em-hint" class="text-xs {invalid.includes('recipients') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('recipients') ? t(errKey.recipients) : t('recipientsHint')}</p>
					</div>
					<div class="space-y-1.5">
						<Label for="em-addr">{t('smtpAddr')}</Label>
						<Input id="em-addr" bind:value={form.addr} autocomplete="off" placeholder="smtp.example.org:587" aria-invalid={invalid.includes('addr')} />
						{#if invalid.includes('addr')}<p class="text-xs text-red-700 dark:text-red-400">{t(errKey.addr)}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="em-from">{t('smtpFrom')}</Label>
						<Input id="em-from" bind:value={form.from} autocomplete="off" aria-invalid={invalid.includes('from')} />
						{#if invalid.includes('from')}<p class="text-xs text-red-700 dark:text-red-400">{t(errKey.from)}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="em-user">{t('smtpUser')}</Label>
						<Input id="em-user" bind:value={form.username} autocomplete="off" />
					</div>
					<div class="space-y-1.5">
						<Label for="em-pw">{t('smtpPassword')}</Label>
						<Input id="em-pw" type="password" bind:value={form.password} autocomplete="new-password" aria-describedby="em-pw-hint" />
						<p id="em-pw-hint" class="text-xs text-muted-foreground">{s.email.passwordSet ? t('passwordSet') : t('passwordNotSet')} {t('secretHint')}</p>
					</div>
					<Button type="button" size="sm" variant="outline" disabled={testing !== undefined} onclick={() => test('email')}>{t('testSend')}</Button>
				</fieldset>
				<p class="text-xs text-muted-foreground">{t('testSaveFirst')}</p>

				<Button type="submit" disabled={saving}>{t('save')}</Button>
				<div aria-live="polite" class="space-y-2">
					{#if saveError}<ApiGate error={saveError} />{/if}
					{#if saved}<Alert.Root><Alert.Description>{t('saved')}</Alert.Description></Alert.Root>{/if}
					{#if testError}<ApiGate error={testError} />{/if}
					{#if tested}<Alert.Root variant={tested.delivered ? 'default' : 'destructive'}><Alert.Description>{tested.delivered ? t('testDelivered') : t('testNotDelivered')}</Alert.Description></Alert.Root>{/if}
				</div>
			</form>
		{/if}
	</Card.Content>
</Card.Root>
