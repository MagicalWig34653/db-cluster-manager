<script lang="ts">
	import { api, type ModuleInfo } from '../../../../lib/api';
	import { dateTime, failureOf, type Failure } from '../../../../lib/format';
	import { load } from '../../../../lib/load.svelte';
	import { locale, t } from '../../../../lib/locale.svelte';
	import { modules } from '../../../../lib/modules.svelte';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import { Badge } from '../../../../lib/components/ui/badge/index.js';
	import ApiGate from '../../../../lib/components/app/ApiGate.svelte';
	import FailureNotice from '../../../../lib/components/app/FailureNotice.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';

	const list = load(api.modules);
	let busy = $state<string | null>(null);
	let failure = $state<Failure | null>(null);

	const name = (m: ModuleInfo) => m.title[locale.value];
	const titleOf = (id: string, all: ModuleInfo[]) => {
		const m = all.find((x) => x.id === id);
		return m ? name(m) : id;
	};

	async function toggle(m: ModuleInfo, enabled: boolean) {
		busy = m.id;
		failure = null;
		try {
			await api.setModule(m.id, enabled);
		} catch (e) {
			failure = failureOf(e);
		} finally {
			busy = null;
			// The answer changes the reasons of the other modules too.
			list.refresh();
			await modules.refresh();
		}
	}
</script>

<PageHeader title={t('modTitle')} intro={t('modIntro')} />

<div aria-live="polite">{#if failure}<FailureNotice {failure} />{/if}</div>

{#if list.state.status === 'loading'}
	<Loading />
{:else if list.state.status === 'error'}
	<ApiGate error={list.state.error} retry={list.reload} />
{:else}
	{@const all = list.state.value.items}
	<ul class="grid gap-3 lg:grid-cols-2">
		{#each all as m (m.id)}
			{@const blocked = m.enabled ? m.dependents.length > 0 : m.missingConfig.length + m.missingModules.length > 0}
			<li>
				<Card.Root class="h-full">
					<Card.Header>
						<Card.Title class="flex flex-wrap items-center gap-2">
							<h2>{name(m)}</h2>
							<Badge variant={m.enabled ? 'default' : 'secondary'}>{m.enabled ? t('modOn') : t('modOff')}</Badge>
						</Card.Title>
						<Card.Description>{m.description[locale.value]}</Card.Description>
					</Card.Header>
					<Card.Content class="space-y-3 text-sm">
						<label class="flex min-h-11 items-center gap-3">
							<input
								type="checkbox"
								role="switch"
								class="size-5 shrink-0 accent-primary"
								checked={m.enabled}
								disabled={busy !== null || (blocked && !m.enabled) || (blocked && m.enabled)}
								onchange={(e) => toggle(m, e.currentTarget.checked)}
							/>
							<span>{t('modToggle', { name: name(m) })}</span>
						</label>
						{#if !m.enabled && m.missingConfig.length}
							<p class="text-muted-foreground">{t('modNeedsConfig', { keys: m.missingConfig.join(', ') })}</p>
						{/if}
						{#if !m.enabled && m.missingModules.length}
							<p class="text-muted-foreground">{t('modNeedsModules', { names: m.missingModules.map((id) => titleOf(id, all)).join(', ') })}</p>
						{/if}
						{#if m.enabled && m.dependents.length}
							<p class="text-muted-foreground">{t('modUsedBy', { names: m.dependents.map((id) => titleOf(id, all)).join(', ') })}</p>
						{/if}
						{#if m.by && m.at}
							<p class="text-xs text-muted-foreground">{t('modChangedBy', { who: m.by, when: dateTime(m.at, locale.value) ?? m.at })}</p>
						{/if}
					</Card.Content>
				</Card.Root>
			</li>
		{/each}
	</ul>
{/if}
