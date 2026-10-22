<script lang="ts">
	import { t } from '../../locale.svelte';
	import { cn } from '../../utils';
	import { statusView, toneClass } from './status';

	let {
		kind = 'state',
		value,
		class: className
	}: { kind?: 'state' | 'operation' | 'severity'; value: string; class?: string } = $props();

	const view = $derived(statusView(kind, value));
</script>

<!-- Icon and text together, so the state never depends on color alone. -->
<span class={cn('inline-flex items-center gap-1.5 font-medium', toneClass[view.tone], className)}>
	<view.icon class="size-4 shrink-0" aria-hidden="true" />
	<span>{t(view.label)}</span>
</span>
