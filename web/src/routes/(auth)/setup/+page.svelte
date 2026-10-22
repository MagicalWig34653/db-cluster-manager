<script lang="ts">
	import { onMount, tick } from 'svelte';
	import Copy from '@lucide/svelte/icons/copy';
	import Download from '@lucide/svelte/icons/download';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import ShieldCheck from '@lucide/svelte/icons/shield-check';
	import { api, ApiError } from '../../../lib/api';
	import { auth } from '../../../lib/auth.svelte';
	import { failureOf, type Failure } from '../../../lib/format';
	import { t } from '../../../lib/locale.svelte';
	import { createPasskey } from '../../../lib/webauthn';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import FailureNotice from '../../../lib/components/app/FailureNotice.svelte';

	let setupToken = $state('');
	let accountId = $state('');
	let displayName = $state('');
	let passkeyLabel = $state('');
	let busy = $state(false);
	let failure = $state<Failure | null>(null);
	/** Set once after a successful setup; the codes are kept only in memory. */
	let result = $state<{ accountId: string; codes: string[] | null } | null>(null);
	let stored = $state(false);
	let copied = $state(false);

	const unavailable = $derived(!result && auth.state.status === 'anonymous' && !auth.state.setupRequired);

	onMount(() => {
		// Optional prefill from a fragment like /setup#token=..., then drop it from the address bar.
		const token = new URLSearchParams(location.hash.slice(1)).get('token');
		if (token) {
			setupToken = token;
			history.replaceState(history.state, '', location.pathname);
		}
	});

	async function submit(ev: SubmitEvent) {
		ev.preventDefault();
		busy = true;
		failure = null;
		try {
			const ch = await api.beginSetup({
				setupToken: setupToken.trim(),
				accountId: accountId.trim(),
				displayName: displayName.trim() || undefined,
				passkeyLabel: passkeyLabel.trim() || undefined
			});
			const credential = await createPasskey(ch.options);
			const res = await api.finishSetup({ setupToken: setupToken.trim(), ceremonyId: ch.ceremonyId, credential });
			setupToken = '';
			result = { accountId: res.accountId, codes: res.recoveryCodes };
			await tick();
			document.getElementById('codes-title')?.focus();
		} catch (e) {
			if (e instanceof ApiError && e.kind === 'unauthenticated') failure = { key: 'setupWrongToken' };
			else if (e instanceof ApiError && e.kind === 'conflict') failure = { key: 'setupUnavailable' };
			else failure = failureOf(e);
		} finally {
			busy = false;
		}
	}

	const codesText = $derived(result?.codes ? result.codes.join('\n') + '\n' : '');

	async function copyCodes() {
		try {
			await navigator.clipboard.writeText(codesText);
			copied = true;
		} catch {
			copied = false;
		}
	}

	function downloadCodes() {
		const url = URL.createObjectURL(new Blob([codesText], { type: 'text/plain' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = `recovery-codes-${result?.accountId ?? 'account'}.txt`;
		a.click();
		URL.revokeObjectURL(url);
	}

	async function finish() {
		// The setup response set a session cookie; the auth layout redirects once it is confirmed.
		result = null;
		await auth.refresh();
	}
</script>

{#if result}
	<Card.Root>
		<Card.Header>
			<h1 id="codes-title" tabindex="-1" class="flex items-center gap-2 text-xl font-semibold outline-none">
				<ShieldCheck class="size-5" aria-hidden="true" />{t('recoveryCodesTitle')}
			</h1>
		</Card.Header>
		<Card.Content class="space-y-4">
			{#if result.codes && result.codes.length > 0}
				<p class="text-sm">{t('recoveryCodesIntro')}</p>
				<ol class="grid grid-cols-2 gap-2 rounded-lg border bg-muted/50 p-3 font-mono text-sm" aria-label={t('recoveryCodesTitle')}>
					{#each result.codes as code, i (i)}<li class="select-all">{code}</li>{/each}
				</ol>
				<div class="flex flex-wrap gap-2">
					<Button variant="outline" size="sm" onclick={copyCodes}><Copy aria-hidden="true" />{t('copyCodes')}</Button>
					<Button variant="outline" size="sm" onclick={downloadCodes}><Download aria-hidden="true" />{t('downloadCodes')}</Button>
					<span class="self-center text-sm text-muted-foreground" aria-live="polite">{copied ? t('copied') : ''}</span>
				</div>
				<label class="flex items-start gap-2 text-sm">
					<input type="checkbox" class="mt-0.5 size-4 accent-[var(--primary)]" bind:checked={stored} />
					{t('codesStored')}
				</label>
			{:else}
				<Alert.Root>
					<TriangleAlert aria-hidden="true" />
					<Alert.Description>{t('recoveryCodesMissing')}</Alert.Description>
				</Alert.Root>
			{/if}
			<Button class="w-full" disabled={!!result.codes?.length && !stored} onclick={finish}>{t('continueToManager')}</Button>
		</Card.Content>
	</Card.Root>
{:else if unavailable}
	<Card.Root>
		<Card.Header>
			<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('setupTitle')}</h1>
		</Card.Header>
		<Card.Content class="space-y-4">
			<p class="text-sm">{t('setupUnavailable')}</p>
			<Button href="/login">{t('loginTitle')}</Button>
		</Card.Content>
	</Card.Root>
{:else}
	<Card.Root>
		<Card.Header>
			<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('setupTitle')}</h1>
			<Card.Description>{t('setupIntro')}</Card.Description>
		</Card.Header>
		<Card.Content>
			<form class="space-y-4" onsubmit={submit}>
				<div class="space-y-1.5">
					<Label for="setup-token">{t('setupToken')}</Label>
					<Input id="setup-token" type="password" bind:value={setupToken} required autocomplete="off" aria-describedby="setup-token-hint" />
					<p id="setup-token-hint" class="text-xs text-muted-foreground">{t('setupTokenHint')}</p>
				</div>
				<div class="space-y-1.5">
					<Label for="account-id">{t('accountName')}</Label>
					<Input
						id="account-id"
						bind:value={accountId}
						required
						maxlength={64}
						pattern="[A-Za-z0-9._@\-]+"
						autocomplete="username"
						aria-describedby="account-id-hint"
					/>
					<p id="account-id-hint" class="text-xs text-muted-foreground">{t('accountNameHint')}</p>
				</div>
				<div class="space-y-1.5">
					<Label for="display-name">{t('displayName')}</Label>
					<Input id="display-name" bind:value={displayName} maxlength={128} autocomplete="name" />
				</div>
				<div class="space-y-1.5">
					<Label for="passkey-label">{t('passkeyLabel')}</Label>
					<Input id="passkey-label" bind:value={passkeyLabel} maxlength={64} autocomplete="off" />
				</div>
				<Button type="submit" class="w-full" disabled={busy}>{t('registerPasskey')}</Button>
				<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>
			</form>
		</Card.Content>
	</Card.Root>
{/if}
