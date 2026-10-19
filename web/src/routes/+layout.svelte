<script lang="ts">
	import '../app.css';
	import { Badge } from '#lib/components/ui/badge/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import { locales } from '#lib/i18n';
	import { locale, t } from '#lib/locale.svelte';

	let { children } = $props();

	$effect(() => {
		document.documentElement.lang = locale.value;
	});
</script>

<div class="min-h-screen">
	<header class="flex items-center gap-3 border-b px-6 py-3">
		<span class="font-semibold">{t('appName')}</span>
		<Badge variant="outline">{t('prototype')}</Badge>
		<nav class="ml-auto flex items-center gap-1" aria-label={t('language')}>
			{#each locales as l (l)}
				<Button
					size="sm"
					variant={locale.value === l ? 'default' : 'ghost'}
					aria-pressed={locale.value === l}
					onclick={() => locale.set(l)}>{l.toUpperCase()}</Button
				>
			{/each}
		</nav>
	</header>
	<main class="mx-auto max-w-3xl p-6">{@render children()}</main>
</div>
