<script lang="ts">
	import type { Snippet } from 'svelte';

	let { title, intro, actions }: { title: string; intro?: string; actions?: Snippet } = $props();
</script>

<header class="mb-6 flex min-w-0 flex-wrap items-start gap-3">
	<div class="min-w-0 flex-1">
		<!-- Focus target after client-side navigation (see the app layout). -->
		<h1 id="page-title" tabindex="-1" class="text-2xl font-semibold tracking-tight outline-none">{title}</h1>
		{#if intro}<p class="mt-1 text-sm text-muted-foreground">{intro}</p>{/if}
	</div>
	{#if actions}<div class="flex max-w-full flex-wrap items-center gap-2">{@render actions()}</div>{/if}
</header>
