<script lang="ts">
	import KeyRound from '@lucide/svelte/icons/key-round';
	import CircleCheck from '@lucide/svelte/icons/circle-check';
	import Sun from '@lucide/svelte/icons/sun';
	import Moon from '@lucide/svelte/icons/moon';
	import Monitor from '@lucide/svelte/icons/monitor';
	import CircleDashed from '@lucide/svelte/icons/circle-dashed';
	import { api } from '../../../lib/api';
	import { load } from '../../../lib/load.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import { auth } from '../../../lib/auth.svelte';
	import { dateTime, failureOf, type Failure } from '../../../lib/format';
	import type { MessageKey } from '../../../lib/i18n';
	import { locale, t } from '../../../lib/locale.svelte';
	import { accents, theme, themeModes, type Accent } from '../../../lib/theme.svelte';
	import { createPasskey } from '../../../lib/webauthn';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import LanguageSwitch from '../../../lib/components/app/LanguageSwitch.svelte';
	import FailureNotice from '../../../lib/components/app/FailureNotice.svelte';
	import { cn } from '../../../lib/utils';

	const version = load(api.version);

	const modeIcons = { system: Monitor, light: Sun, dark: Moon };
	const modeLabels: Record<(typeof themeModes)[number], MessageKey> = {
		system: 'themeSystem',
		light: 'themeLight',
		dark: 'themeDark'
	};
	const accentLabels: Record<Accent, MessageKey> = {
		zinc: 'accentZinc',
		blue: 'accentBlue',
		green: 'accentGreen',
		violet: 'accentViolet',
		orange: 'accentOrange'
	};
	const swatch: Record<Accent, string> = {
		zinc: 'bg-zinc-800 dark:bg-zinc-200',
		blue: 'bg-blue-600',
		green: 'bg-emerald-600',
		violet: 'bg-violet-600',
		orange: 'bg-orange-600'
	};

	let label = $state('');
	let busy = $state(false);
	let failure = $state<Failure | null>(null);
	let added = $state<string | null>(null);

	async function addPasskey(ev: SubmitEvent) {
		ev.preventDefault();
		busy = true;
		failure = null;
		added = null;
		try {
			const ch = await api.beginPasskey(label.trim() || undefined);
			const credential = await createPasskey(ch.options);
			const res = await api.finishPasskey({ ceremonyId: ch.ceremonyId, credential });
			added = res.passkey.label ? t('passkeyAdded', { label: res.passkey.label }) : t('passkeyAddedUnnamed');
			label = '';
			// The credential change replaced this browser's session cookie.
			await auth.refresh();
		} catch (e) {
			failure = failureOf(e);
		} finally {
			busy = false;
		}
	}
</script>

<PageHeader title={t('settingsTitle')} intro={t('settingsIntro')} />

<div class="grid gap-4 lg:grid-cols-2">
	<Card.Root>
		<Card.Header>
			<Card.Title><h2>{t('appearance')}</h2></Card.Title>
			<Card.Description>{t('preferencesLocal')}</Card.Description>
		</Card.Header>
		<Card.Content class="space-y-5">
			<fieldset>
				<legend class="mb-2 text-sm font-medium">{t('language')}</legend>
				<LanguageSwitch />
			</fieldset>
			<fieldset>
				<legend class="mb-2 text-sm font-medium">{t('theme')}</legend>
				<div class="flex flex-wrap gap-2" role="group">
					{#each themeModes as m (m)}
						{@const Icon = modeIcons[m]}
						<Button variant={theme.mode === m ? 'secondary' : 'outline'} size="sm" aria-pressed={theme.mode === m} onclick={() => theme.setMode(m)}>
							<Icon aria-hidden="true" />{t(modeLabels[m])}
						</Button>
					{/each}
				</div>
			</fieldset>
			<fieldset>
				<legend class="mb-2 text-sm font-medium">{t('accent')}</legend>
				<div class="flex flex-wrap gap-2" role="group">
					{#each accents as a (a)}
						<Button variant={theme.accent === a ? 'secondary' : 'outline'} size="sm" aria-pressed={theme.accent === a} onclick={() => theme.setAccent(a)}>
							<span class={cn('inline-block size-3 rounded-full', swatch[a])} aria-hidden="true"></span>{t(accentLabels[a])}
						</Button>
					{/each}
				</div>
			</fieldset>
		</Card.Content>
	</Card.Root>

	{#if auth.state.status === 'signedIn'}
		{@const s = auth.state.session}
		<Card.Root>
			<Card.Header>
				<Card.Title><h2>{t('account')}</h2></Card.Title>
			</Card.Header>
			<Card.Content class="space-y-5">
				<dl class="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
					<dt class="text-muted-foreground">{t('accountId')}</dt>
					<dd class="font-mono">{s.accountId}</dd>
					<dt class="text-muted-foreground">{t('sessionSince')}</dt>
					<dd>{dateTime(s.createdAt, locale.value)}</dd>
				</dl>
				<form class="space-y-3" onsubmit={addPasskey}>
					<h3 class="flex items-center gap-2 text-sm font-medium"><KeyRound class="size-4" aria-hidden="true" />{t('addPasskey')}</h3>
					<p class="text-sm text-muted-foreground">{t('addPasskeyHint')}</p>
					<div class="space-y-1.5">
						<Label for="pk-label">{t('passkeyLabel')}</Label>
						<Input id="pk-label" bind:value={label} maxlength={64} autocomplete="off" />
					</div>
					<Button type="submit" disabled={busy}>{t('addPasskey')}</Button>
					<div aria-live="polite">
						{#if failure}<FailureNotice {failure} />{/if}
						{#if added}
							<Alert.Root>
								<CircleCheck aria-hidden="true" />
								<Alert.Description>{added}</Alert.Description>
							</Alert.Root>
						{/if}
					</div>
				</form>
			</Card.Content>
		</Card.Root>
	{/if}

	<Card.Root class="lg:col-span-2">
		<Card.Header>
			<Card.Title><h2>{t('managerSettings')}</h2></Card.Title>
		</Card.Header>
		<Card.Content>
			{#if version.state.status === 'ok'}
				<dl class="grid grid-cols-[8rem_1fr] gap-y-2 text-sm">
					<dt class="text-muted-foreground">{t('version')}</dt><dd class="font-mono">{version.state.value.version}</dd>
					<dt class="text-muted-foreground">Commit</dt><dd class="font-mono">{version.state.value.commit}</dd>
					<dt class="text-muted-foreground">Go</dt><dd class="font-mono">{version.state.value.goVersion}</dd>
					<dt class="text-muted-foreground">{t('platform')}</dt><dd class="font-mono">{version.state.value.platform}</dd>
				</dl>
			{:else if version.state.status === 'error'}
				<ApiGate error={version.state.error} retry={version.reload} />
			{/if}
			<p class="mt-4 flex items-center gap-2 text-sm text-muted-foreground">
				<CircleDashed class="size-4" aria-hidden="true" />{t('managerSettingsEditNA')}
			</p>
			<p class="mt-2 text-sm"><a class="underline underline-offset-4" href="/notifications/settings">{t('notifySettingsTitle')}</a></p>
		</Card.Content>
	</Card.Root>
</div>
