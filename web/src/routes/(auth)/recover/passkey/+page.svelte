<script lang="ts">
	import KeyRound from '@lucide/svelte/icons/key-round';
	import { api } from '../../../../lib/api';
	import { auth } from '../../../../lib/auth.svelte';
	import { failureOf, type Failure } from '../../../../lib/format';
	import { t } from '../../../../lib/locale.svelte';
	import { createPasskey } from '../../../../lib/webauthn';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import FailureNotice from '../../../../lib/components/app/FailureNotice.svelte';

	let label = $state('');
	let busy = $state(false);
	let failure = $state<Failure | null>(null);

	const account = $derived(auth.state.status === 'signedIn' ? auth.state.session.accountId : null);

	async function enroll(ev: SubmitEvent) {
		ev.preventDefault();
		busy = true;
		failure = null;
		try {
			const ch = await api.beginPasskey(label.trim() || undefined);
			const credential = await createPasskey(ch.options);
			await api.finishPasskey({ ceremonyId: ch.ceremonyId, credential });
			// A full session replaced the recovery session; the auth layout redirects.
			await auth.refresh();
		} catch (e) {
			failure = failureOf(e);
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('enrollTitle')}</h1>
	</Card.Header>
	<Card.Content>
		{#if account}
			<form class="space-y-4" onsubmit={enroll}>
				<p class="text-sm">{t('enrollIntro', { account })}</p>
				<div class="space-y-1.5">
					<Label for="enroll-label">{t('passkeyLabel')}</Label>
					<Input id="enroll-label" bind:value={label} maxlength={64} autocomplete="off" />
				</div>
				<Button type="submit" class="w-full" disabled={busy}><KeyRound aria-hidden="true" />{t('enrollButton')}</Button>
				<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>
			</form>
		{:else}
			<!-- No recovery session (expired or never started). -->
			<div class="space-y-4">
				<p class="text-sm">{t('errUnauthenticated')}</p>
				<Button href="/recover" variant="outline">{t('recoverTitle')}</Button>
			</div>
		{/if}
	</Card.Content>
</Card.Root>
