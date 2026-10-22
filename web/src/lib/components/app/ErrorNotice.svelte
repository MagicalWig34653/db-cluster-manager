<script lang="ts">
	import CircleX from '@lucide/svelte/icons/circle-x';
	import type { ApiError } from '../../api';
	import { errorMessage } from '../../format';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import { Button } from '../ui/button/index.js';

	let { error, retry }: { error: ApiError; retry?: () => void } = $props();
	const msg = $derived(errorMessage(error));
</script>

<Alert.Root variant="destructive">
	<CircleX aria-hidden="true" />
	<Alert.Description class="flex flex-wrap items-center gap-3">
		<span>{t(msg.key, msg.params)}</span>
		{#if retry}<Button size="sm" variant="outline" onclick={retry}>{t('retry')}</Button>{/if}
	</Alert.Description>
</Alert.Root>
