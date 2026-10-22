<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ApiError } from '../../../../lib/api';
	import { auth } from '../../../../lib/auth.svelte';
	import { failureOf, recoveryLinkParams, type Failure } from '../../../../lib/format';
	import { t } from '../../../../lib/locale.svelte';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import FailureNotice from '../../../../lib/components/app/FailureNotice.svelte';

	// Link format (proposed): /recover/link#account=<id>&token=<token>.
	// The fragment is never sent to the server; it is read once and removed
	// from the address bar. The single-use token is redeemed only on an
	// explicit click, so link previews or prefetchers cannot consume it.
	let account = $state<string | undefined>();
	let token = $state<string | undefined>();
	let ready = $state(false);
	let busy = $state(false);
	let failure = $state<Failure | null>(null);

	onMount(() => {
		({ account, token } = recoveryLinkParams(new URL(location.href)));
		if (location.hash || location.search) history.replaceState(history.state, '', location.pathname);
		ready = true;
	});

	async function redeem() {
		if (!account || !token) return;
		busy = true;
		failure = null;
		try {
			await api.redeemRecoveryToken(account, token);
			token = undefined;
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
		<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('recoverTokenTitle')}</h1>
	</Card.Header>
	<Card.Content class="space-y-4">
		{#if !ready}
			<span class="sr-only">{t('loading')}</span>
		{:else if account && token}
			<p class="text-sm">{t('recoverTokenIntro', { account })}</p>
			<Button class="w-full" onclick={redeem} disabled={busy}>{t('redeemToken')}</Button>
		{:else if !failure}
			<p class="text-sm">{t('recoverTokenMissing')}</p>
		{/if}
		<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>
		<p class="text-sm"><a class="text-muted-foreground underline underline-offset-4" href="/login">{t('backToLogin')}</a></p>
	</Card.Content>
</Card.Root>
