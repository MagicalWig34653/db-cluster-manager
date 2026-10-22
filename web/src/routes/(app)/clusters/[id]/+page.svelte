<script lang="ts">
	import { page } from '$app/state';
	import ClusterDetail from '../../../../lib/components/app/ClusterDetail.svelte';

	const id = $derived(page.params.id ?? '');
</script>

{#key id}
	<ClusterDetail {id} />
{/key}
