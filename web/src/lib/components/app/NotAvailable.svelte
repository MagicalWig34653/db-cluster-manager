<script lang="ts">
	import CircleDashed from '@lucide/svelte/icons/circle-dashed';
	import { t } from '../../locale.svelte';
	import type { MessageKey } from '../../i18n';
	import PageHeader from './PageHeader.svelte';

	let { title }: { title: MessageKey } = $props();
</script>

<PageHeader title={t(title)} />
<section class="rounded-lg border border-dashed p-8 text-center" aria-labelledby="na-title">
	<CircleDashed class="mx-auto mb-3 size-8 text-muted-foreground" aria-hidden="true" />
	<h2 id="na-title" class="font-medium">{t('notAvailableTitle')}</h2>
	<p class="mx-auto mt-1 max-w-prose text-sm text-muted-foreground">{t('notAvailableBody')}</p>
</section>
