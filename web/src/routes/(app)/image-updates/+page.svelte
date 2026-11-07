<script lang="ts">
	import { api, asApiError, type ApiError, type ImageUpdateImage, type ImageUpdatePlan } from '../../../lib/api';
	import { planActions, policySummary, type ImageUpdateAction } from '../../../lib/image-update';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime } from '../../../lib/format';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';

	const services = load(api.imageUpdateServices);
	const plans = load(api.imageUpdatePlans);
	let busy = $state('');
	let message = $state('');
	let error = $state<ApiError | undefined>();
	let open = $state('');
	let detail = $state<ImageUpdatePlan | undefined>();

	async function run(key: string, fn: () => Promise<unknown>) {
		busy = key; error = undefined; message = '';
		try { await fn(); services.refresh(); plans.refresh(); }
		catch (e) { error = asApiError(e); }
		finally { busy = ''; }
	}
	const check = (id: string) => run('check' + id, async () => { message = (await api.checkImageUpdate(id)).message ?? ''; });
	const act = (p: ImageUpdatePlan, what: ImageUpdateAction) =>
		run(p.id + what, () => what === 'approve' ? api.approveImageUpdatePlan(p.id, p.digest) : what === 'apply' ? api.applyImageUpdatePlan(p.id) : what === 'decline' ? api.declineImageUpdatePlan(p.id) : api.rollbackImageUpdatePlan(p.id));
	async function show(id: string) {
		if (open === id) { open = ''; return; }
		open = id; detail = undefined; error = undefined;
		try { detail = await api.imageUpdatePlan(id); }
		catch (e) { error = asApiError(e); }
	}
	const image = (i: ImageUpdateImage | undefined) => i?.tag ?? i?.digest?.slice(0, 19) ?? '—';
	const label: Record<ImageUpdateAction, 'iuApprove' | 'iuApply' | 'iuDecline' | 'iuRollback'> = { approve: 'iuApprove', apply: 'iuApply', decline: 'iuDecline', rollback: 'iuRollback' };
</script>

<PageHeader title={t('iuTitle')} intro={t('iuIntro')} />
{#if message}<p class="text-sm" role="status">{message}</p>{/if}
{#if error}<p class="text-sm text-destructive" role="alert">{error.message}</p>{/if}

<section class="space-y-3" aria-labelledby="iu-services">
	<h2 id="iu-services" class="text-lg font-medium">{t('iuServices')}</h2>
	{#if services.state.status === 'loading'}<Loading />
	{:else if services.state.status === 'error'}<ApiGate error={services.state.error} retry={services.reload} />
	{:else if !services.state.value.length}<p class="text-sm text-muted-foreground">{t('iuNoServices')}</p>
	{:else}
		<div class="overflow-x-auto">
		<Table.Root>
			<Table.Header><Table.Row>
				<Table.Head>{t('iuService')}</Table.Head><Table.Head>{t('iuPolicy')}</Table.Head><Table.Head>{t('iuLastCheck')}</Table.Head><Table.Head></Table.Head>
			</Table.Row></Table.Header>
			<Table.Body>
				{#each services.state.value as s (s.service.id)}
					<Table.Row>
						<Table.Cell><div class="font-medium">{s.service.name}</div>
							<div class="font-mono text-xs text-muted-foreground">{s.service.image ?? '—'}{#if s.database} · {t('iuDatabase')}{/if}</div>
							{#if s.service.foreign}<div class="text-xs text-amber-700 dark:text-amber-400">{t('iuForeign')}</div>{/if}</Table.Cell>
						<Table.Cell>{policySummary(s.policy)}</Table.Cell>
						<Table.Cell>{dateTime(s.state?.lastCheckAt, locale.value) ?? '—'}{#if s.state?.lastError}<div class="text-xs text-destructive">{s.state.lastError}</div>{/if}</Table.Cell>
						<Table.Cell>{#if !s.service.foreign}<Button size="sm" variant="outline" disabled={busy !== ''} onclick={() => check(s.service.id)}>{t('iuCheck')}</Button>{/if}</Table.Cell>
					</Table.Row>
				{/each}
			</Table.Body>
		</Table.Root>
		</div>
	{/if}
</section>

<section class="mt-8 space-y-3" aria-labelledby="iu-plans">
	<h2 id="iu-plans" class="text-lg font-medium">{t('iuPlans')}</h2>
	{#if plans.state.status === 'loading'}<Loading />
	{:else if plans.state.status === 'error'}<ApiGate error={plans.state.error} retry={plans.reload} />
	{:else if !plans.state.value.length}<p class="text-sm text-muted-foreground">{t('iuNoPlans')}</p>
	{:else}
		<div class="overflow-x-auto">
		<Table.Root>
			<Table.Header><Table.Row>
				<Table.Head>{t('iuService')}</Table.Head><Table.Head>{t('iuChange')}</Table.Head><Table.Head>{t('iuState')}</Table.Head><Table.Head>{t('iuCreated')}</Table.Head><Table.Head></Table.Head>
			</Table.Row></Table.Header>
			<Table.Body>
				{#each plans.state.value as p (p.id)}
					<Table.Row>
						<Table.Cell>{p.serviceName ?? p.serviceId}</Table.Cell>
						<Table.Cell><div class="font-mono text-sm">{image(p.from)} → {image(p.to)}</div>
							<div class="text-xs text-muted-foreground">{p.kind ?? '—'}{#if p.auto} · {t('iuAuto')}{/if}</div>
							{#if p.kind === 'built'}<div class="text-xs text-amber-700 dark:text-amber-400">{t('iuBuilt')}</div>{/if}
							{#if p.state === 'proposed' && p.route === 'planner'}<div class="text-xs text-amber-700 dark:text-amber-400">{t('iuPlannerOnly')}</div>{/if}
							{#if p.state === 'proposed' && p.route === 'report-only'}<div class="text-xs text-amber-700 dark:text-amber-400">{t('iuReportOnly')}</div>{/if}</Table.Cell>
						<Table.Cell>{t(`iuPlan_${p.state}`)}{#if p.error}<div class="text-xs text-destructive">{p.error}</div>{/if}</Table.Cell>
						<Table.Cell>{dateTime(p.createdAt, locale.value) ?? '—'}</Table.Cell>
						<Table.Cell class="space-x-2 whitespace-nowrap">
							<Button size="sm" variant="ghost" aria-expanded={open === p.id} aria-controls="iu-detail-{p.id}" onclick={() => show(p.id)}>{t('iuDetails')}</Button>
							{#each planActions(p) as a (a)}
								<Button size="sm" variant={a === 'approve' || a === 'apply' ? 'default' : 'outline'} disabled={busy !== ''} onclick={() => act(p, a)}>{t(label[a])}</Button>
							{/each}
						</Table.Cell>
					</Table.Row>
					{#if open === p.id}
						<Table.Row id="iu-detail-{p.id}"><Table.Cell colspan={5}>
							{#if detail?.id === p.id}
								<dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
									<dt class="text-muted-foreground">{t('iuKind')}</dt><dd>{detail.kind ?? '—'}</dd>
									<dt class="text-muted-foreground">{t('iuSource')}</dt><dd>{detail.source ?? '—'}</dd>
									<dt class="text-muted-foreground">{t('iuRoute')}</dt><dd>{detail.route ?? '—'}</dd>
									<dt class="text-muted-foreground">{t('iuDigest')}</dt><dd class="break-all font-mono text-xs">{detail.digest}</dd>
									<dt class="text-muted-foreground">{t('iuApprovedBy')}</dt><dd>{detail.approvedBy ?? '—'}</dd>
									<dt class="text-muted-foreground">{t('iuOperation')}</dt><dd>{#if detail.operationId}<a class="underline" href="/operations">{detail.operationId}</a>{:else}—{/if}</dd>
									<dt class="text-muted-foreground">{t('iuExecutedBy')}</dt><dd>{detail.executedBy ?? '—'}</dd>
								</dl>
							{:else}<Loading />{/if}
						</Table.Cell></Table.Row>
					{/if}
				{/each}
			</Table.Body>
		</Table.Root>
		</div>
	{/if}
</section>
