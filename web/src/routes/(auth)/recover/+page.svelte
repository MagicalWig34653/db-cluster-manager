<script lang="ts">
	import { api, ApiError } from '../../../lib/api';
	import { auth } from '../../../lib/auth.svelte';
	import { failureOf, type Failure } from '../../../lib/format';
	import { t } from '../../../lib/locale.svelte';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import FailureNotice from '../../../lib/components/app/FailureNotice.svelte';

	let accountId = $state('');
	let code = $state('');
	let busy = $state(false);
	let failure = $state<Failure | null>(null);

	async function submit(ev: SubmitEvent) {
		ev.preventDefault();
		busy = true;
		failure = null;
		try {
			await api.redeemRecoveryCode(accountId.trim(), code.trim());
			code = '';
			// A recovery session now exists; the auth layout moves on to passkey enrolment.
			await auth.refresh();
		} catch (e) {
			failure = e instanceof ApiError && e.kind === 'unauthenticated' ? { key: 'recoveryFailed' } : failureOf(e);
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('recoverTitle')}</h1>
		<Card.Description>{t('recoverIntro')}</Card.Description>
	</Card.Header>
	<Card.Content>
		<form class="space-y-4" onsubmit={submit}>
			<div class="space-y-1.5">
				<Label for="rec-account">{t('accountName')}</Label>
				<Input id="rec-account" bind:value={accountId} required maxlength={64} autocomplete="username" />
			</div>
			<div class="space-y-1.5">
				<Label for="rec-code">{t('recoveryCode')}</Label>
				<Input id="rec-code" bind:value={code} required autocomplete="one-time-code" spellcheck={false} class="font-mono" />
			</div>
			<Button type="submit" class="w-full" disabled={busy}>{t('redeemCode')}</Button>
			<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>
			<p class="text-sm"><a class="text-muted-foreground underline underline-offset-4" href="/login">{t('backToLogin')}</a></p>
		</form>
	</Card.Content>
</Card.Root>
