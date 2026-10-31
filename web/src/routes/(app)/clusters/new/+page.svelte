<script lang="ts">
	import { api, asApiError, type Host, type Swarm } from '../../../../lib/api';
	import { buildRequest, emptyForm, placementModes, preflight, validate, type ClusterForm } from '../../../../lib/cluster';
	import { load } from '../../../../lib/load.svelte';
	import { t } from '../../../../lib/locale.svelte';
	import type { MessageKey } from '../../../../lib/i18n';
	import { checkClass, selectClass } from '../../../../lib/components/app/fieldClass';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../../lib/components/app/ApiGate.svelte';
	import ErrorNotice from '../../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';
	import FormErrors from '../../../../lib/components/app/FormErrors.svelte';
	import { focusFirstInvalid } from '../../../../lib/components/app/form-focus';

	// POST /api/v1/clusters with api.CreateClusterRequest (lib/cluster.ts
	// buildRequest) answers 202 { id, kind } of the started operation. Coded
	// refusals (no_capacity, policy_conflict, settings_incomplete,
	// cluster_exists, busy, not_leader) are mapped by errorMessage. Hosts for
	// the preflight come from GET /api/v1/hosts.
	const swarms = load(api.swarms);
	const hosts = load(api.hosts);

	const steps = ['stepBasics', 'stepPlacement', 'stepResources', 'stepPreflight', 'stepReview'] as const;
	let step = $state(0);
	let form = $state<ClusterForm>(emptyForm());
	let busy = $state(false);
	let created = $state<{ id: string; kind?: string } | undefined>();
	let submitError = $state<ReturnType<typeof asApiError> | undefined>();
	let shown = $state(false); // show validation errors after the first attempt to proceed
	let wizard = $state<HTMLDivElement | null>(null);

	const errors = $derived(validate(form));
	const errFor = (f: keyof ClusterForm) => (shown ? errors.find((e) => e.field === f)?.key : undefined);
	const hostList = $derived<Host[]>(hosts.state.status === 'ok' ? hosts.state.value.filter((h) => h.swarm === form.swarm) : []);
	const pf = $derived(hosts.state.status === 'ok' ? preflight(form, hosts.state.value) : undefined);
	const modeHint: Record<string, MessageKey> = {
		automatic: 'modeAutomaticHint',
		exclude: 'modeExcludeHint',
		prefer: 'modePreferHint',
		allowlist: 'modeAllowlistHint'
	};
	const modeLabel: Record<string, MessageKey> = {
		automatic: 'modeAutomatic',
		exclude: 'modeExclude',
		prefer: 'modePrefer',
		allowlist: 'modeAllowlist'
	};
	// Fields per step so "Continue" only blocks on the current step.
	const stepFields: (keyof ClusterForm)[][] = [
		['swarm', 'name', 'image', 'network', 'rootPasswordSecret'],
		['hosts'],
		['cpuLimit', 'memLimit', 'cpuReservation', 'memReservation', 'reserveCpu', 'reserveMem'],
		[],
		[]
	];
	function next() {
		shown = true;
		if (errors.some((e) => stepFields[step].includes(e.field))) {
			void focusFirstInvalid(wizard);
			return;
		}
		shown = false;
		step = Math.min(step + 1, steps.length - 1);
	}
	function toggleHost(id: string) {
		form.hosts = form.hosts.includes(id) ? form.hosts.filter((h) => h !== id) : [...form.hosts, id];
	}
	async function create() {
		shown = true;
		if (errors.length > 0) {
			void focusFirstInvalid(wizard);
			return;
		}
		busy = true;
		submitError = undefined;
		try {
			created = await api.createCluster(buildRequest(form));
		} catch (e) {
			submitError = asApiError(e);
		} finally {
			busy = false;
		}
	}
</script>

{#snippet field(id: string, label: MessageKey, key: keyof ClusterForm, hint?: MessageKey, placeholder = '')}
	{@const err = errFor(key)}
	<div class="space-y-1.5">
		<Label for={id}>{t(label)}</Label>
		<Input
			{id}
			{placeholder}
			value={form[key] as string}
			oninput={(e) => ((form[key] as string) = e.currentTarget.value)}
			aria-invalid={err ? true : undefined}
			aria-describedby={err ? `${id}-err` : hint ? `${id}-hint` : undefined}
			autocomplete="off"
		/>
		{#if hint}<p id="{id}-hint" class="text-xs text-muted-foreground">{t(hint)}</p>{/if}
		{#if err}<p id="{id}-err" class="text-xs text-red-700 dark:text-red-400">{t(err)}</p>{/if}
	</div>
{/snippet}

<PageHeader title={t('newCluster')} intro={t('wizardIntro')} />

{#if created}
	<Alert.Root>
		<Alert.Title>{t('createStarted')}</Alert.Title>
		<Alert.Description>
			<p>{t('createStartedBody', { id: created.id })}</p>
			<p class="mt-2 flex gap-4 text-sm">
				<a class="underline underline-offset-4" href="/operations#{encodeURIComponent(created.id)}">{t('navOperations')}</a>
				<a class="underline underline-offset-4" href="/clusters">{t('navClusters')}</a>
			</p>
		</Alert.Description>
	</Alert.Root>
{:else}
	<ol class="mb-6 flex flex-wrap gap-2 text-sm" aria-label={t('wizardSteps')}>
		{#each steps as s, i (s)}
			<li aria-current={i === step ? 'step' : undefined} class={i === step ? 'font-semibold' : 'text-muted-foreground'}>
				{i + 1}. {t(s)}{i < steps.length - 1 ? ' ›' : ''}
			</li>
		{/each}
	</ol>

	<div bind:this={wizard}>
	<FormErrors errors={shown ? errors.filter((error) => stepFields[step].includes(error.field)).map((error) => error.key) : []} />
	<Card.Root>
		<Card.Content class="space-y-5">
			{#if step === 0}
				<div class="space-y-1.5">
					<Label for="f-swarm">{t('colSwarm')}</Label>
					{#if swarms.state.status === 'loading'}
						<Loading />
					{:else if swarms.state.status === 'error'}
						<ErrorNotice error={swarms.state.error} retry={swarms.reload} />
					{:else}
						<select id="f-swarm" class={selectClass} bind:value={form.swarm} aria-invalid={errFor('swarm') ? true : undefined}>
							<option value="">{t('choose')}</option>
							{#each swarms.state.value as s (s.id)}<option value={s.id}>{(s as Swarm).name}</option>{/each}
						</select>
						{#if errFor('swarm')}<p class="text-xs text-red-700 dark:text-red-400">{t(errFor('swarm')!)}</p>{/if}
					{/if}
				</div>
				{@render field('f-name', 'colName', 'name', 'clusterNameHint')}
				<fieldset class="space-y-1.5">
					<legend class="text-sm font-medium">{t('topology')}</legend>
					<div class="flex gap-2" role="group">
						{#each [3, 5] as n (n)}
							<Button variant={form.size === n ? 'secondary' : 'outline'} size="sm" aria-pressed={form.size === n} onclick={() => (form.size = n as 3 | 5)}>
								{t('nodeCount', { count: n })}
							</Button>
						{/each}
					</div>
					<p class="text-xs text-muted-foreground">{t('topologyHint')}</p>
				</fieldset>
				{@render field('f-image', 'image', 'image', undefined, 'mariadb:11')}
				{@render field('f-network', 'network', 'network', 'networkHint')}
				{@render field('f-secret', 'rootSecret', 'rootPasswordSecret', 'rootSecretHint')}
				{@render field('f-gcache', 'gcache', 'gcacheSize', 'optional', '512M')}
				<label class="flex items-center gap-2 text-sm">
					<input type="checkbox" class={checkClass} bind:checked={form.autoReplace} />{t('autoReplace')}
				</label>
				<p class="-mt-3 text-xs text-muted-foreground">{t('autoReplaceHint')}</p>
				<label class="flex items-center gap-2 text-sm">
					<input type="checkbox" class={checkClass} bind:checked={form.binlog} aria-describedby="f-binlog-hint" />{t('binlogOption')}
				</label>
				<p id="f-binlog-hint" class="-mt-3 text-xs text-muted-foreground">{t('binlogOptionHint')}</p>
			{:else if step === 1}
				<fieldset class="space-y-2">
					<legend class="text-sm font-medium">{t('placementMode')}</legend>
					{#each placementModes as m (m)}
						<label class="flex items-start gap-2 text-sm">
							<input type="radio" name="mode" value={m} class="mt-1" bind:group={form.mode} />
							<span><span class="font-medium">{t(modeLabel[m])}</span> — <span class="text-muted-foreground">{t(modeHint[m])}</span></span>
						</label>
					{/each}
				</fieldset>
				<div class="rounded-md bg-muted p-3 text-sm">
					<p class="font-medium">{t('precedenceTitle')}</p>
					<ol class="mt-1 list-decimal space-y-0.5 pl-5 text-muted-foreground">
						<li>{t('precedence1')}</li>
						<li>{t('precedence2')}</li>
						<li>{t('precedence3')}</li>
						<li>{t('precedence4')}</li>
					</ol>
				</div>
				{#if form.mode !== 'automatic'}
					<fieldset class="space-y-2" tabindex="-1" data-invalid={errFor('hosts') ? 'true' : undefined} aria-describedby={errFor('hosts') ? 'hosts-error' : undefined}>
						<legend class="text-sm font-medium">{t('pickHosts')}</legend>
						{#if hosts.state.status === 'loading'}
							<Loading />
						{:else if hosts.state.status === 'error'}
							<ApiGate error={hosts.state.error} retry={hosts.reload} />
						{:else if hostList.length === 0}
							<p class="text-sm text-muted-foreground">{t('hostsEmpty')}</p>
						{:else}
							{#each hostList as h (h.nodeId)}
								<label class="flex items-center gap-2 text-sm">
									<input type="checkbox" class={checkClass} checked={form.hosts.includes(h.nodeId)} onchange={() => toggleHost(h.nodeId)} />
									{h.hostname ?? h.nodeId}
								</label>
							{/each}
						{/if}
						{#if errFor('hosts')}<p id="hosts-error" class="text-xs text-red-700 dark:text-red-400">{t(errFor('hosts')!)}</p>{/if}
					</fieldset>
				{/if}
			{:else if step === 2}
				<p class="text-sm text-muted-foreground">{t('resourcesHint')}</p>
				<div class="grid gap-4 sm:grid-cols-2">
					{@render field('f-cl', 'cpuLimit', 'cpuLimit', undefined, '2')}
					{@render field('f-ml', 'memLimit', 'memLimit', undefined, '4')}
					{@render field('f-cr', 'cpuReservation', 'cpuReservation', undefined, '1')}
					{@render field('f-mr', 'memReservation', 'memReservation', undefined, '2')}
				</div>
				<h3 class="text-sm font-medium">{t('hostReserve')}</h3>
				<p class="-mt-3 text-xs text-muted-foreground">{t('hostReserveHint')}</p>
				<div class="grid gap-4 sm:grid-cols-2">
					{@render field('f-rc', 'reserveCpu', 'reserveCpu', undefined, '0.5')}
					{@render field('f-rm', 'reserveMem', 'reserveMem', undefined, '1')}
				</div>
			{:else if step === 3}
				<h2 class="font-medium">{t('stepPreflight')}</h2>
				<p class="text-sm text-muted-foreground">{t('preflightHint')}</p>
				{#if hosts.state.status === 'loading'}
					<Loading />
				{:else if hosts.state.status === 'error'}
					<ApiGate error={hosts.state.error} retry={hosts.reload} />
					<p class="text-sm">{t('preflightSkipped')}</p>
				{:else if pf}
					{#if pf.verdicts.length === 0}
						<p class="text-sm text-muted-foreground">{t('hostsEmpty')}</p>
					{:else}
						<ul class="space-y-1 text-sm">
							{#each pf.verdicts as v (v.id)}
								<li>
									<span class="font-mono text-xs">{hostList.find((h) => h.nodeId === v.id)?.hostname ?? v.id}</span>:
									<span class="font-medium">{v.eligible ? t('pfEligible') : t('pfRejected')}</span>
									{#if !v.eligible} — <span class="text-muted-foreground">{t(v.reason)}</span>{/if}
								</li>
							{/each}
						</ul>
						<p class="text-sm font-medium" role="status">
							{pf.enough ? t('pfEnough', { eligible: pf.eligible, size: form.size }) : t('pfNotEnough', { eligible: pf.eligible, size: form.size })}
						</p>
					{/if}
					<p class="text-xs text-muted-foreground">{t('pfCaveat')}</p>
				{/if}
			{:else}
				<h2 class="font-medium">{t('stepReview')}</h2>
				<dl class="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-2 text-sm">
					<dt class="text-muted-foreground">{t('colSwarm')}</dt><dd class="font-mono text-xs">{form.swarm}</dd>
					<dt class="text-muted-foreground">{t('colName')}</dt><dd>{form.name}</dd>
					<dt class="text-muted-foreground">{t('topology')}</dt><dd>{t('nodeCount', { count: form.size })}</dd>
					<dt class="text-muted-foreground">{t('image')}</dt><dd class="font-mono text-xs">{form.image}</dd>
					<dt class="text-muted-foreground">{t('placementMode')}</dt>
					<dd>{t(modeLabel[form.mode])}{form.hosts.length ? ` (${form.hosts.join(', ')})` : ''}</dd>
					<dt class="text-muted-foreground">{t('autoReplace')}</dt><dd>{form.autoReplace ? t('yes') : t('no')}</dd>
					<dt class="text-muted-foreground">{t('binlogOption')}</dt><dd>{form.binlog ? t('yes') : t('no')}</dd>
				</dl>
				<details>
					<summary class="cursor-pointer text-sm">{t('requestBody')}</summary>
					<pre class="mt-2 overflow-x-auto rounded-md bg-muted p-3 text-xs">{JSON.stringify(buildRequest(form), null, 2)}</pre>
				</details>
				<p class="text-xs text-muted-foreground">{t('reviewHint')}</p>
				{#if submitError}
					<div aria-live="polite">
						{#if submitError.code}
							<ErrorNotice error={submitError} />
						{:else}
							<ApiGate error={submitError} />
						{/if}
					</div>
				{/if}
			{/if}

			<div class="flex flex-wrap gap-2 pt-2">
				{#if step > 0}<Button variant="outline" onclick={() => (step -= 1)}>{t('back')}</Button>{/if}
				{#if step < steps.length - 1}
					<Button onclick={next}>{t('continue')}</Button>
				{:else}
					<Button onclick={create} disabled={busy || errors.length > 0}>{t('createCluster')}</Button>
				{/if}
				<Button variant="ghost" href="/clusters">{t('cancel')}</Button>
			</div>
		</Card.Content>
	</Card.Root>
	</div>
{/if}
