<script lang="ts">
	import { getHealth, getVersion, type VersionInfo } from '#lib/api';
	import * as Card from '#lib/components/ui/card/index.js';
	import { t } from '#lib/locale.svelte';

	type State<T> = { state: 'loading' } | { state: 'error'; message: string } | { state: 'ok'; value: T };

	let health = $state<State<{ status: string }>>({ state: 'loading' });
	let info = $state<State<VersionInfo>>({ state: 'loading' });

	$effect(() => {
		const toErr = (e: unknown) => ({ state: 'error' as const, message: e instanceof Error ? e.message : String(e) });
		getHealth().then(
			(value) => (health = { state: 'ok', value }),
			(e) => (health = toErr(e))
		);
		getVersion().then(
			(value) => (info = { state: 'ok', value }),
			(e) => (info = toErr(e))
		);
	});
</script>

<h1 class="mb-1 text-2xl font-semibold">{t('title')}</h1>
<p class="mb-6 text-sm text-muted-foreground">{t('intro')}</p>

<Card.Root>
	<Card.Header>
		<Card.Title>{t('processStatus')}</Card.Title>
	</Card.Header>
	<Card.Content>
		<dl class="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
			<dt class="text-muted-foreground">{t('apiHealth')}</dt>
			<dd>
				{#if health.state === 'loading'}{t('checking')}{:else if health.state === 'error'}{t('unavailable')} ({health.message}){:else if health.value.status === 'ok'}{t('healthOk')}{:else}{health.value.status}{/if}
			</dd>
			{#if info.state === 'ok'}
				<dt class="text-muted-foreground">{t('version')}</dt><dd>{info.value.version}</dd>
				<dt class="text-muted-foreground">{t('commit')}</dt><dd>{info.value.commit}</dd>
				<dt class="text-muted-foreground">{t('go')}</dt><dd>{info.value.goVersion}</dd>
				<dt class="text-muted-foreground">{t('platform')}</dt><dd>{info.value.platform}</dd>
			{:else if info.state === 'error'}
				<dt class="text-muted-foreground">{t('version')}</dt><dd>{t('unavailable')} ({info.message})</dd>
			{/if}
		</dl>
	</Card.Content>
</Card.Root>
