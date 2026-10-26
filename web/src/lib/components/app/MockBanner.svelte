<!-- Development only: imported dynamically behind import.meta.env.DEV, never bundled in production. -->
<script lang="ts">
	import { t } from '../../locale.svelte';
</script>
<div
	class="sticky top-0 z-50 border-b-2 border-amber-600 bg-amber-300 px-4 py-1.5 text-center text-sm font-bold text-black"
	role="note"
>
	{t('mockBannerTitle')} — <span class="font-normal">{t('mockBannerBody')}</span>
</div>
