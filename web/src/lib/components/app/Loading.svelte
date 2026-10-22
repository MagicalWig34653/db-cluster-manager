<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { t } from '../../locale.svelte';
</script>

<p class="flex items-center gap-2 text-sm text-muted-foreground" role="status">
	<LoaderCircle class="size-4 motion-safe:animate-spin" aria-hidden="true" />{t('loading')}
</p>
