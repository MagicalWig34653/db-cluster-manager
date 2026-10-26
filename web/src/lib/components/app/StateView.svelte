<script lang="ts">
	import CircleDashed from '@lucide/svelte/icons/circle-dashed';
	import CircleX from '@lucide/svelte/icons/circle-x';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import type { MessageKey, Params } from '../../i18n';
	import { t } from '../../locale.svelte';
	import { Button } from '../ui/button/index.js';

	let { kind, title, message, params, retry }: {
		kind: 'empty' | 'error' | 'loading';
		title?: MessageKey;
		message?: MessageKey;
		params?: Params;
		retry?: () => void;
	} = $props();
</script>

<div
	role={kind === 'error' ? 'alert' : 'status'}
	aria-live={kind === 'error' ? 'assertive' : 'polite'}
	class="flex min-w-0 items-start gap-3 rounded-lg border p-4 text-sm {kind === 'error' ? 'border-destructive text-foreground' : 'border-border text-foreground'}"
>
	{#if kind === 'loading'}<LoaderCircle class="mt-0.5 size-4 shrink-0 motion-safe:animate-spin" aria-hidden="true" />
	{:else if kind === 'error'}<CircleX class="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
	{:else}<CircleDashed class="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden="true" />{/if}
	<div class="min-w-0 space-y-1">
		{#if title}<p class="font-medium">{t(title)}</p>{/if}
		{#if message}<p class="break-words">{t(message, params)}</p>{/if}
		{#if retry}<Button size="sm" variant="outline" onclick={retry}>{t('retry')}</Button>{/if}
	</div>
</div>
