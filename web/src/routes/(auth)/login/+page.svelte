<script lang="ts">
	import KeyRound from '@lucide/svelte/icons/key-round';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '../../../lib/api';
	import { auth } from '../../../lib/auth.svelte';
	import { failureOf, type Failure } from '../../../lib/format';
	import { t } from '../../../lib/locale.svelte';
	import { getPasskey } from '../../../lib/webauthn';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import FailureNotice from '../../../lib/components/app/FailureNotice.svelte';

	let busy = $state(false);
	let failure = $state<Failure | null>(null);

	$effect(() => {
		if (auth.state.status === 'anonymous' && auth.state.setupRequired) void goto('/setup', { replaceState: true });
	});

	async function signIn() {
		busy = true;
		failure = null;
		try {
			const ch = await api.beginLogin();
			const credential = await getPasskey(ch.options);
			await api.finishLogin({ ceremonyId: ch.ceremonyId, credential });
			await auth.refresh();
		} catch (e) {
			failure = e instanceof ApiError && e.kind === 'unauthenticated' ? { key: 'loginFailed' } : failureOf(e);
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<h1 id="page-title" tabindex="-1" class="text-xl font-semibold outline-none">{t('loginTitle')}</h1>
		<Card.Description>{t('loginIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="space-y-4">
		<Button class="w-full" size="lg" onclick={signIn} disabled={busy}>
			<KeyRound aria-hidden="true" />{t('loginButton')}
		</Button>
		<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>
		<p class="text-sm"><a class="text-muted-foreground underline underline-offset-4" href="/recover">{t('lostPasskey')}</a></p>
	</Card.Content>
</Card.Root>
