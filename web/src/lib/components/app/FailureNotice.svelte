<script lang="ts">
	import CircleX from '@lucide/svelte/icons/circle-x';
	import type { Failure } from '../../format';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';

	let { failure }: { failure: Failure } = $props();
</script>

<Alert.Root variant="destructive">
	<CircleX aria-hidden="true" />
	<Alert.Description>{t(failure.key, failure.params)}</Alert.Description>
</Alert.Root>
