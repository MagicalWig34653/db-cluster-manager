<script lang="ts">
	import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
	import { dateTime, relativeAge } from '../../format';
	import { locale, t } from '../../locale.svelte';

	// plain: the time is not an observation (a backup, an expiry, a next run): show the bare relative time.
	let { at, plain = false }: { at?: string; plain?: boolean } = $props();
	const age = $derived(relativeAge(at, locale.value));
</script>

{#if age && at}
	<time datetime={at} title={dateTime(at, locale.value)} class="text-muted-foreground">{plain ? age : t('observedAgo', { age })}</time>
{:else}
	<span class="inline-flex items-center gap-1 text-muted-foreground">
		<CircleQuestionMark class="size-3.5" aria-hidden="true" />{t('freshnessUnknown')}
	</span>
{/if}
