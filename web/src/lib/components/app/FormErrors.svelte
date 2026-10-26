<script lang="ts">
	import type { MessageKey } from '../../i18n';
	import { t } from '../../locale.svelte';
	let { errors }: { errors: MessageKey[] } = $props();
</script>

{#if errors.length}
	<div role="alert" class="rounded-md border border-destructive p-3 text-sm">
		<p class="font-medium">{t('formErrorSummary')}</p>
		<ul class="mt-1 list-disc pl-5">
			{#each [...new Set(errors)] as key (key)}<li>{t(key)}</li>{/each}
		</ul>
	</div>
{/if}
