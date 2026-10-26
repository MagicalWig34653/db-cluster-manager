<script lang="ts">
	import type { ApiError } from '../../api';
	import { errorMessage } from '../../format';
	import StateView from './StateView.svelte';

	let { error, retry }: { error: ApiError; retry?: () => void } = $props();
	const msg = $derived(errorMessage(error));
</script>

<StateView kind="error" message={msg.key} params={msg.params} {retry} />
