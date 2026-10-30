<script lang="ts">
	import { afterNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import Menu from '@lucide/svelte/icons/menu';
	import LogOut from '@lucide/svelte/icons/log-out';
	import X from '@lucide/svelte/icons/x';
	import Info from '@lucide/svelte/icons/info';
	import { auth } from '../../lib/auth.svelte';
	import { modules } from '../../lib/modules.svelte';
	import { t } from '../../lib/locale.svelte';
	import { Button } from '../../lib/components/ui/button/index.js';
	import * as Sheet from '../../lib/components/ui/sheet/index.js';
	import * as Alert from '../../lib/components/ui/alert/index.js';
	import NavList from '../../lib/components/app/NavList.svelte';
	import LanguageSwitch from '../../lib/components/app/LanguageSwitch.svelte';
	import Loading from '../../lib/components/app/Loading.svelte';
	import ErrorNotice from '../../lib/components/app/ErrorNotice.svelte';

	let { children } = $props();
	let drawerOpen = $state(false);

	const st = $derived(auth.state);

	$effect(() => {
		if (st.status === 'anonymous') void goto(st.setupRequired ? '/setup' : '/login', { replaceState: true });
		else if (st.status === 'signedIn' && st.session.restricted) void goto('/recover/passkey', { replaceState: true });
	});

	$effect(() => {
		if (st.status === 'signedIn' && !st.session.restricted) void modules.refresh();
	});

	// After client-side navigation, move focus to the new page's heading so
	// keyboard and screen-reader users start at the content.
	afterNavigate((nav) => {
		drawerOpen = false;
		if (nav.type === 'enter') return;
		requestAnimationFrame(() => document.getElementById('page-title')?.focus());
	});

	// Without auth configuration only process status exists; other pages would only show 404s.
	const disabledAllowed = $derived(page.url.pathname === '/' || page.url.pathname === '/settings');

	async function signOut() {
		await auth.logout();
	}
</script>

<a
	href="#main"
	class="sr-only z-50 rounded-md bg-background px-3 py-2 focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:ring-2 focus:ring-ring"
	>{t('skipToContent')}</a
>

{#snippet brand()}
	<a href="/" class="flex items-center gap-2 rounded-md px-1 font-semibold outline-none focus-visible:ring-2 focus-visible:ring-ring">
		<img src="/favicon.svg" alt="" class="size-6 shrink-0" aria-hidden="true" />
		{t('appName')}
	</a>
{/snippet}

<div class="min-h-screen lg:grid lg:grid-cols-[15rem_1fr]">
	<!-- Fixed desktop sidebar -->
	<aside class="sticky top-0 hidden h-screen flex-col gap-4 border-r bg-card/60 p-3 backdrop-blur supports-[backdrop-filter]:bg-card/40 lg:flex">
		<div class="px-1 pt-1">{@render brand()}</div>
		<NavList />
	</aside>

	<div class="flex min-w-0 flex-col">
		<!-- Top bar -->
		<header class="sticky top-0 z-30 flex h-14 items-center gap-2 border-b bg-background/85 px-3 backdrop-blur supports-[backdrop-filter]:bg-background/70 sm:px-4">
			<Sheet.Root bind:open={drawerOpen}>
				<Sheet.Trigger>
					{#snippet child({ props })}
						<Button {...props} variant="ghost" size="icon" class="lg:hidden" aria-label={t('openNavigation')}>
							<Menu aria-hidden="true" />
						</Button>
					{/snippet}
				</Sheet.Trigger>
				<Sheet.Content side="left" showCloseButton={false} class="max-w-[calc(100vw-1rem)] w-72 overflow-y-auto p-3">
					<div class="flex items-center justify-between">
						<Sheet.Title class="text-base">{@render brand()}</Sheet.Title>
						<Sheet.Description class="sr-only">{t('mainNavigation')}</Sheet.Description>
						<Sheet.Close>
							{#snippet child({ props })}
								<Button {...props} variant="ghost" size="icon-sm" aria-label={t('closeNavigation')}><X aria-hidden="true" /></Button>
							{/snippet}
						</Sheet.Close>
					</div>
					<NavList onnavigate={() => (drawerOpen = false)} />
				</Sheet.Content>
			</Sheet.Root>
			<div class="lg:hidden">{@render brand()}</div>
			<div class="ml-auto flex items-center gap-2">
				<LanguageSwitch />
				{#if st.status === 'signedIn'}
					<span class="hidden text-sm text-muted-foreground md:inline">{t('signedInAs', { account: st.session.accountId })}</span>
					<Button variant="ghost" size="sm" onclick={signOut}>
						<LogOut aria-hidden="true" /><span class="hidden sm:inline">{t('signOut')}</span><span class="sr-only sm:hidden">{t('signOut')}</span>
					</Button>
				{/if}
			</div>
		</header>

		<main id="main" tabindex="-1" class="mx-auto w-full min-w-0 max-w-6xl flex-1 p-4 sm:p-6">
			{#if st.status === 'loading' || st.status === 'anonymous'}
				<Loading />
			{:else if st.status === 'error'}
				<ErrorNotice error={st.error} retry={auth.refresh} />
			{:else if st.status === 'signedIn' && st.session.restricted}
				<Loading />
			{:else}
				{#if st.status === 'disabled'}
					<Alert.Root class="mb-6">
						<Info aria-hidden="true" />
						<Alert.Description>{t('authDisabled')}</Alert.Description>
					</Alert.Root>
				{/if}
				{#if st.status === 'signedIn' || disabledAllowed}
					{@render children()}
				{/if}
			{/if}
		</main>
	</div>
</div>
