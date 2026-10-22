<script lang="ts">
	import Plus from '@lucide/svelte/icons/plus';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import { api, asApiError, metricFuncs, panelKinds, type ApiError, type Dashboard, type DashboardInput } from '../../../lib/api';
	import { auth } from '../../../lib/auth.svelte';
	import { canEditDashboard, defaultPanel, sortPanels, spanOf, validateDashboard } from '../../../lib/dashboard';
	import type { MessageKey } from '../../../lib/i18n';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import { checkClass, selectClass } from '../../../lib/components/app/fieldClass';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import PanelView from '../../../lib/components/app/PanelView.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	// GET /dashboards, POST /dashboards/{id} (create, client-chosen id),
	// PUT/DELETE /dashboards/{id}; panels query POST /metrics/query. Personal vs
	// shared is the boolean `shared`; only the owner edits, never a template.
	const accountId = $derived(auth.state.status === 'signedIn' ? auth.state.session.accountId : undefined);
	const kindLabel = { line: 'panelLine', stat: 'panelStat', table: 'panelTable' } as const;
	const dashboards = load(api.dashboards);
	let selected = $state<string | undefined>();
	let editing = $state<{ id?: string; draft: DashboardInput } | undefined>();
	let error = $state<ApiError | undefined>();
	let busy = $state(false);

	const current = $derived(dashboards.state.status === 'ok' ? dashboards.state.value.find((d) => d.id === selected) : undefined);
	const personal = $derived(dashboards.state.status === 'ok' ? dashboards.state.value.filter((d) => !d.shared) : []);
	const shared = $derived(dashboards.state.status === 'ok' ? dashboards.state.value.filter((d) => d.shared) : []);

	const newPanel = (index: number) => defaultPanel(index, crypto.randomUUID());
	function edit(d?: Dashboard) {
		editing = d
			? { id: d.id, draft: { title: d.title, shared: d.shared, panels: d.panels.map((p) => ({ ...p })) } }
			: { draft: { title: '', shared: false, panels: [newPanel(0)] } };
		error = undefined;
		shown = false;
	}
	let shown = $state(false);
	const issues = $derived(editing ? validateDashboard(editing.draft) : []);
	const issueFor = (field: string, panel?: number): MessageKey | undefined =>
		shown ? issues.find((i) => i.field === field && i.panel === panel)?.key : undefined;
	const valid = $derived(issues.length === 0);
	async function save(e: SubmitEvent) {
		e.preventDefault();
		shown = true;
		if (!editing || !valid) return;
		busy = true;
		error = undefined;
		try {
			const saved = await api.saveDashboard(editing.draft, editing.id);
			editing = undefined;
			selected = saved.id;
			dashboards.reload();
		} catch (err) {
			error = asApiError(err);
		} finally {
			busy = false;
		}
	}
	async function remove(id: string) {
		busy = true;
		try {
			await api.deleteDashboard(id);
			selected = undefined;
			dashboards.reload();
		} catch (err) {
			error = asApiError(err);
		} finally {
			busy = false;
		}
	}
</script>

{#snippet group(title: 'personalDashboards' | 'sharedDashboards', list: Dashboard[])}
	<section aria-labelledby="g-{title}">
		<h2 id="g-{title}" class="mb-1 text-sm font-medium">{t(title)}</h2>
		{#if list.length === 0}
			<p class="text-sm text-muted-foreground">{t('none')}</p>
		{:else}
			<ul>
				{#each list as d (d.id)}
					<li>
						<button
							type="button"
							class="w-full rounded-md px-2 py-1.5 text-left text-sm outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring {selected === d.id && !editing ? 'bg-primary/10 font-medium' : ''}"
							aria-current={selected === d.id && !editing ? 'true' : undefined}
							onclick={() => ((selected = d.id), (editing = undefined))}>{d.title}</button>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
{/snippet}

<PageHeader title={t('navDashboards')} intro={t('dashboardsIntro')}>
	{#snippet actions()}<Button onclick={() => edit()}><Plus aria-hidden="true" />{t('newDashboard')}</Button>{/snippet}
</PageHeader>

{#if dashboards.state.status === 'loading'}
	<Loading />
{:else if dashboards.state.status === 'error'}
	<ApiGate error={dashboards.state.error} retry={dashboards.reload} />
{:else}
	<div class="grid gap-6 lg:grid-cols-[14rem_1fr]">
		<div class="space-y-4">
			{@render group('personalDashboards', personal)}
			{@render group('sharedDashboards', shared)}
		</div>
		<div>
			{#if editing}
				<form class="space-y-4" onsubmit={save}>
					<Card.Root>
						<Card.Content class="space-y-4">
							<div class="space-y-1.5">
								<Label for="d-name">{t('panelTitle')}</Label>
								<Input id="d-name" bind:value={editing.draft.title} autocomplete="off" aria-invalid={issueFor('title') ? true : undefined} />
								{#if issueFor('title')}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('title')!)}</p>{/if}
							</div>
							<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={editing.draft.shared} />{t('shareDashboard')}</label>
							{#each editing.draft.panels as p, i (p.id)}
								<fieldset class="space-y-3 rounded-md border p-3">
									<legend class="px-1 text-sm font-medium">{t('panelN', { n: i + 1 })}</legend>
									<div class="grid gap-3 sm:grid-cols-2">
										<div class="space-y-1.5">
											<Label for="p-t-{p.id}">{t('panelTitle')}</Label>
											<Input id="p-t-{p.id}" bind:value={p.title} autocomplete="off" aria-invalid={issueFor('panelTitle', i) ? true : undefined} />
											{#if issueFor('panelTitle', i)}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('panelTitle', i)!)}</p>{/if}
										</div>
										<div class="space-y-1.5">
											<Label for="p-k-{p.id}">{t('panelType')}</Label>
											<select id="p-k-{p.id}" class={selectClass} bind:value={p.kind}>
												{#each panelKinds as k (k)}<option value={k}>{t(kindLabel[k])}</option>{/each}
											</select>
										</div>
										<div class="space-y-1.5 sm:col-span-2">
											<Label for="p-q-{p.id}">{t('panelSelector')}</Label>
											<Input id="p-q-{p.id}" bind:value={p.selector} class="font-mono" autocomplete="off" aria-invalid={issueFor('selector', i) ? true : undefined} />
											{#if issueFor('selector', i)}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('selector', i)!)}</p>{/if}
										</div>
										<div class="space-y-1.5">
											<Label for="p-f-{p.id}">{t('panelFunc')}</Label>
											<select id="p-f-{p.id}" class={selectClass} bind:value={p.func}>
												{#each metricFuncs as f (f)}<option value={f}>{f}</option>{/each}
											</select>
										</div>
										{#if p.func === 'quantile'}
											<div class="space-y-1.5">
												<Label for="p-qu-{p.id}">{t('panelQuantile')}</Label>
												<Input id="p-qu-{p.id}" type="number" min="0" max="1" step="0.01" bind:value={p.quantile} aria-invalid={issueFor('quantile', i) ? true : undefined} />
												{#if issueFor('quantile', i)}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('quantile', i)!)}</p>{/if}
											</div>
										{/if}
										<div class="space-y-1.5">
											<Label for="p-s-{p.id}">{t('panelStep')}</Label>
											<Input id="p-s-{p.id}" type="number" min="1" step="1" bind:value={p.stepSeconds} aria-invalid={issueFor('step', i) ? true : undefined} />
											{#if issueFor('step', i)}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('step', i)!)}</p>{/if}
										</div>
										<div class="space-y-1.5">
											<Label for="p-w-{p.id}">{t('panelWindow')}</Label>
											<Input id="p-w-{p.id}" type="number" min="0" step="1" bind:value={p.windowSeconds} aria-invalid={issueFor('window', i) ? true : undefined} />
											{#if issueFor('window', i)}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('window', i)!)}</p>{/if}
										</div>
									</div>
									<Button type="button" variant="ghost" size="sm" onclick={() => editing && (editing.draft.panels = editing.draft.panels.filter((x) => x.id !== p.id))}>
										<Trash2 aria-hidden="true" />{t('removePanel')}
									</Button>
								</fieldset>
							{/each}
							<Button type="button" variant="outline" size="sm" onclick={() => editing && editing.draft.panels.push(newPanel(editing.draft.panels.length))}><Plus aria-hidden="true" />{t('addPanel')}</Button>
							<div class="flex gap-2">
								<Button type="submit" disabled={busy}>{t('save')}</Button>
								<Button type="button" variant="ghost" onclick={() => (editing = undefined)}>{t('cancel')}</Button>
							</div>
							{#if shown && issueFor('panels')}<p class="text-xs text-red-700 dark:text-red-400">{t(issueFor('panels')!)}</p>{/if}
							<div aria-live="polite">{#if error}<ApiGate {error} />{/if}</div>
						</Card.Content>
					</Card.Root>
				</form>
			{:else if current}
				<div class="mb-4 flex flex-wrap items-center gap-2">
					<h2 class="text-lg font-semibold">{current.title}</h2>
					<span class="text-xs text-muted-foreground">({current.shared ? t('sharedDashboards') : t('personalDashboards')}{current.template ? `, ${t('dashboardTemplate')}` : ''})</span>
					<span class="flex-1"></span>
					{#if canEditDashboard(current, accountId)}
						<Button size="sm" variant="outline" onclick={() => edit(current)}>{t('edit')}</Button>
						<Button size="sm" variant="outline" disabled={busy} onclick={() => remove(current.id)}>{t('delete')}</Button>
					{/if}
				</div>
				{#if !canEditDashboard(current, accountId)}<p class="mb-3 text-xs text-muted-foreground">{t('dashboardReadOnly')}</p>{/if}
				{#if current.panels.length === 0}
					<p class="text-sm text-muted-foreground">{t('noPanels')}</p>
				{:else}
					<!-- 24-column grid from the panel layout on wide screens; one column on narrow ones. -->
					<div class="grid gap-4 lg:grid-cols-24">
						{#key current.id}{#each sortPanels(current.panels) as p (p.id)}<div class="min-w-0 lg:[grid-column:span_var(--span)]" style="--span: {spanOf(p)}"><PanelView panel={p} /></div>{/each}{/key}
					</div>
				{/if}
				<div aria-live="polite">{#if error}<ApiGate {error} />{/if}</div>
			{:else}
				<p class="text-sm text-muted-foreground">{dashboards.state.value.length === 0 ? t('dashboardsEmpty') : t('selectDashboard')}</p>
			{/if}
		</div>
	</div>
{/if}
