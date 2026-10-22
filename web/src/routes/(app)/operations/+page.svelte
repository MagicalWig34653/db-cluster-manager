<script lang="ts">
	import { api } from '../../../lib/api';
	import { dateTime, newestFirst, relativeAge } from '../../../lib/format';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Status from '../../../lib/components/app/Status.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const ops = load(api.operations);
	const sorted = $derived(ops.state.status === 'ok' ? newestFirst(ops.state.value, (o) => o.startedAt) : []);
</script>

<PageHeader title={t('operationsTitle')} intro={t('operationsIntro')} />

{#if ops.state.status === 'loading'}
	<Loading />
{:else if ops.state.status === 'error'}
	<ErrorNotice error={ops.state.error} retry={ops.reload} />
{:else if sorted.length === 0}
	<p class="text-sm text-muted-foreground">{t('operationsEmpty')}</p>
{:else}
	<ol class="relative ml-2 border-l pl-6">
		{#each sorted as o (o.id)}
			<li class="relative pb-6 last:pb-0">
				<span class="absolute top-1.5 -left-[1.95rem] size-3 rounded-full border-2 border-background bg-border" aria-hidden="true"></span>
				<div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
					<span class="font-medium">{o.kind}</span>
					<Status kind="operation" value={o.state} />
				</div>
				<p class="mt-0.5 text-sm text-muted-foreground">
					{#if o.cluster}<a class="underline-offset-4 hover:underline" href="/clusters/{encodeURIComponent(o.cluster)}">{o.cluster}</a>{/if}
					{#if o.swarm}<span class="font-mono text-xs">{o.swarm}</span>{/if}
					{#if !o.cluster && !o.swarm}{t('managerLevel')}{/if}
				</p>
				<p class="mt-0.5 text-xs text-muted-foreground">
					{#if o.startedAt}
						{t('started')}: <time datetime={o.startedAt} title={dateTime(o.startedAt, locale.value)}>{relativeAge(o.startedAt, locale.value)}</time>
					{:else}
						{t('startUnknown')}
					{/if}
					{#if o.endedAt}
						· {t('ended')}: <time datetime={o.endedAt} title={dateTime(o.endedAt, locale.value)}>{relativeAge(o.endedAt, locale.value)}</time>
					{/if}
				</p>
			</li>
		{/each}
	</ol>
{/if}
