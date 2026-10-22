<script lang="ts">
	import '../app.css';
	import { mockActive } from '../lib/api';
	import { auth } from '../lib/auth.svelte';
	import { locale } from '../lib/locale.svelte';
	import { theme } from '../lib/theme.svelte';

	let { children } = $props();

	// Statically false in production builds, so the banner module is not bundled.
	const mockBanner = import.meta.env.DEV && mockActive ? import('../lib/components/app/MockBanner.svelte') : null;

	$effect(() => {
		document.documentElement.lang = locale.value;
	});
	$effect(() => {
		theme.apply();
		void auth.refresh();
	});
</script>

{#if mockBanner}
	{#await mockBanner then M}<M.default />{/await}
{/if}
{@render children()}
