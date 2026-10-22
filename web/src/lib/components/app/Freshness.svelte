<script lang="ts">
	import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
	import { dateTime, relativeAge } from '../../format';
	import { locale, t } from '../../locale.svelte';

	let { at }: { at?: string } = $props();
	const age = $derived(relativeAge(at, locale.value));
</script>

{#if age && at}
	<time datetime={at} title={dateTime(at, locale.value)} class="text-muted-foreground">{t('observedAgo', { age })}</time>
{:else}
	<span class="inline-flex items-center gap-1 text-muted-foreground">
		<CircleQuestionMark class="size-3.5" aria-hidden="true" />{t('freshnessUnknown')}
	</span>
{/if}
