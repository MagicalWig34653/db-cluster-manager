<script lang="ts">
	import { afterNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import Info from '@lucide/svelte/icons/info';
	import { auth } from '../../lib/auth.svelte';
	import { t } from '../../lib/locale.svelte';
	import * as Alert from '../../lib/components/ui/alert/index.js';
	import LanguageSwitch from '../../lib/components/app/LanguageSwitch.svelte';
	import Loading from '../../lib/components/app/Loading.svelte';
	import ErrorNotice from '../../lib/components/app/ErrorNotice.svelte';

	let { children } = $props();
	const st = $derived(auth.state);

	$effect(() => {
		if (st.status !== 'signedIn') return;
		if (st.session.restricted) {
			if (page.url.pathname !== '/recover/passkey') void goto('/recover/passkey', { replaceState: true });
		} else {
			void goto('/', { replaceState: true });
		}
	});

	afterNavigate((nav) => {
		if (nav.type !== 'enter') requestAnimationFrame(() => document.getElementById('page-title')?.focus());
	});
</script>

<div class="flex min-h-screen flex-col">
	<header class="flex h-14 items-center gap-2 border-b px-4">
		<span class="flex items-center gap-2 font-semibold">
			<img src="/favicon.svg" alt="" class="size-6 shrink-0" aria-hidden="true" />{t('appName')}
		</span>
		<div class="ml-auto"><LanguageSwitch /></div>
	</header>
	<main id="main" class="mx-auto w-full max-w-lg flex-1 p-4 sm:py-12">
		{#if st.status === 'loading'}
			<Loading />
		{:else if st.status === 'error'}
			<ErrorNotice error={st.error} retry={auth.refresh} />
		{:else if st.status === 'disabled'}
			<Alert.Root>
				<Info aria-hidden="true" />
				<Alert.Description>
					{t('authDisabled')}
					<a class="underline underline-offset-4" href="/">{t('navOverview')}</a>
				</Alert.Description>
			</Alert.Root>
		{:else}
			{@render children()}
		{/if}
	</main>
</div>
