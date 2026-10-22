<script lang="ts">
	import { locales } from '../../i18n';
	import { locale, t } from '../../locale.svelte';
	import { Button } from '../ui/button/index.js';
</script>

<div class="flex items-center gap-1" role="group" aria-label={t('language')}>
	{#each locales as l (l)}
		<Button
			size="sm"
			variant={locale.value === l ? 'secondary' : 'ghost'}
			aria-pressed={locale.value === l}
			lang={l}
			onclick={() => locale.set(l)}>{l.toUpperCase()}</Button
		>
	{/each}
</div>
