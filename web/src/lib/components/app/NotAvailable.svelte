<script lang="ts">
	import { t } from '../../locale.svelte';
	import type { MessageKey } from '../../i18n';
	import PageHeader from './PageHeader.svelte';
	import StateView from './StateView.svelte';

	let { title }: { title: MessageKey } = $props();
</script>

<PageHeader title={t(title)} />
<StateView kind="empty" title="notAvailableTitle" message="notAvailableBody" />
