<script lang="ts">
	import '../app.css';
	let { children } = $props();
</script>

<div class="min-h-screen bg-zinc-50 text-zinc-900 dark:bg-zinc-950 dark:text-zinc-100">
	<header class="flex items-center gap-3 border-b border-zinc-200 px-6 py-3 dark:border-zinc-800">
		<span class="font-semibold">DB Cluster Manager</span>
		<span class="rounded-full border border-amber-500 px-2 py-0.5 text-xs text-amber-700 dark:text-amber-400">Prototype</span>
	</header>
	<main class="mx-auto max-w-3xl p-6">{@render children()}</main>
</div>
