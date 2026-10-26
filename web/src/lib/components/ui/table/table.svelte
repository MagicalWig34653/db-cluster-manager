<script lang="ts">
	import { cn, type WithElementRef } from "#lib/utils.js";
	import { t } from "../../../locale.svelte";
	import type { HTMLTableAttributes } from "svelte/elements";

	let {
		ref = $bindable(null),
		class: className,
		children,
		...restProps
	}: WithElementRef<HTMLTableAttributes> = $props();
</script>

<!-- A focusable scroll region lets keyboard users reach columns on narrow screens. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div data-slot="table-container" tabindex="0" role="region" aria-label={t('scrollableTable')} class="relative w-full max-w-full overflow-x-auto focus-visible:outline-2 focus-visible:outline-ring">
	<table bind:this={ref} data-slot="table" class={cn("w-full caption-bottom text-sm", className)} {...restProps}>
		{@render children?.()}
	</table>
</div>
