<script lang="ts">
	import { api, asApiError, type ApiError, type ServiceInfo, type ServicePlan, type ServiceStarted, type ServiceType } from '../../../lib/api';
	import { buildServiceInput, emptyServiceForm, logLevels, planView, validateServiceForm, type RestartPolicy, type ServiceFormError } from '../../../lib/services';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import { checkClass, selectClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import PlanBlock from '../../../lib/components/app/PlanBlock.svelte';
	import FollowOperation from '../../../lib/components/app/FollowOperation.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const services = load(api.services);
	const swarms = load(api.swarms);
	const hosts = load(api.hosts);

	let form = $state(emptyServiceForm('container'));
	let invalid = $state<ServiceFormError[]>([]);
	// The plan that was shown; editing the form afterwards invalidates it.
	let shown = $state<{ plan: ServicePlan; request: string } | undefined>();
	let planBusy = $state(false);
	let planError = $state<ApiError | undefined>();
	let requestBusy = $state(false);
	let started = $state<ServiceStarted | undefined>();
	let requestError = $state<ApiError | undefined>();

	const requestJson = $derived(JSON.stringify(buildServiceInput(form)));
	const stale = $derived(shown !== undefined && shown.request !== requestJson);
	const hostChoices = $derived(hosts.state.status === 'ok' ? hosts.state.value.filter((h) => h.swarm === form.swarm && h.enrollment === 'enrolled') : []);

	const err = (k: ServiceFormError, key: MessageKey) => (invalid.includes(k) ? t(key) : undefined);
	const typeLabel = (ty: string): MessageKey => (ty === 'traefik' ? 'svcTypeTraefik' : 'svcTypeContainer');
	const restartLabel: Record<RestartPolicy, MessageKey> = { any: 'svcRestartAny', 'on-failure': 'svcRestartOnFailure', none: 'svcRestartNone' };

	function setType(ty: ServiceType) {
		const keep = { swarm: form.swarm, name: form.name };
		form = { ...emptyServiceForm(ty), ...keep };
		shown = undefined;
		started = undefined;
		invalid = [];
	}
	async function preview(e: SubmitEvent) {
		e.preventDefault();
		invalid = validateServiceForm(form);
		started = undefined;
		if (invalid.length > 0) return;
		planBusy = true;
		planError = undefined;
		try {
			shown = { plan: await api.previewService(buildServiceInput(form)), request: requestJson };
		} catch (x) {
			shown = undefined;
			planError = asApiError(x);
		} finally {
			planBusy = false;
		}
	}
	async function request() {
		if (!shown || stale) return;
		requestBusy = true;
		requestError = undefined;
		try {
			started = await api.createService(buildServiceInput(form));
			services.refresh();
		} catch (x) {
			requestError = asApiError(x);
		} finally {
			requestBusy = false;
		}
	}

	// Deletion: show the plan first (with or without the data volumes), then request it.
	let deleting = $state<string | undefined>();
	let removeVolumes = $state(false);
	let delPlan = $state<ServicePlan | undefined>();
	let delBusy = $state(false);
	let delStarted = $state<ServiceStarted | undefined>();
	let delError = $state<ApiError | undefined>();
	function startDelete(s: ServiceInfo) {
		deleting = s.id;
		removeVolumes = false;
		delPlan = undefined;
		delStarted = undefined;
		delError = undefined;
	}
	async function showDeletePlan(id: string) {
		delBusy = true;
		delError = undefined;
		delStarted = undefined;
		try {
			delPlan = await api.previewServiceDelete(id, removeVolumes);
		} catch (x) {
			delPlan = undefined;
			delError = asApiError(x);
		} finally {
			delBusy = false;
		}
	}
	async function requestDelete(id: string) {
		delBusy = true;
		delError = undefined;
		try {
			delStarted = await api.deleteService(id, removeVolumes);
			services.refresh();
		} catch (x) {
			delError = asApiError(x);
		} finally {
			delBusy = false;
		}
	}
	const imageOf = (s: ServiceInfo) => (typeof s.spec.image === 'string' ? s.spec.image : undefined);
</script>

<PageHeader title={t('svcTitle')} intro={t('svcIntro')} />

<ol class="mb-4 flex flex-wrap gap-x-3 gap-y-1 text-sm text-muted-foreground" aria-label={t('wizardSteps')}>
	<li>{t('svcFlow1')} ›</li><li>{t('svcFlow2')} ›</li><li>{t('svcFlow3')}</li>
</ol>

<div class="space-y-6">
	{#if services.state.status === 'loading'}
		<Loading />
	{:else if services.state.status === 'error'}
		<ApiGate error={services.state.error} retry={services.reload} />
	{:else if services.state.value.length === 0}
		<p class="text-sm text-muted-foreground">{t('svcEmpty')}</p>
	{:else}
		<div class="rounded-lg border">
			<Table.Root>
				<Table.Header>
					<Table.Row>
						<Table.Head>{t('svcColName')}</Table.Head>
						<Table.Head>{t('svcColType')}</Table.Head>
						<Table.Head>{t('colSwarm')}</Table.Head>
						<Table.Head>{t('svcColImage')}</Table.Head>
						<Table.Head>{t('svcColCreated')}</Table.Head>
						<Table.Head>{t('colActions')}</Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each services.state.value as s (s.id)}
						<Table.Row>
							<Table.Cell class="font-medium">{s.name}</Table.Cell>
							<Table.Cell>{t(typeLabel(s.type))}</Table.Cell>
							<Table.Cell class="font-mono text-xs">{s.swarm}</Table.Cell>
							<Table.Cell class="font-mono text-xs">{imageOf(s) ?? t('notReported')}</Table.Cell>
							<Table.Cell><Freshness at={s.createdAt} /></Table.Cell>
							<Table.Cell>
								{#if s.deleteOperation}
									<span class="text-xs text-muted-foreground">{t('svcDeletePending')}</span>
								{:else}
									<Button size="sm" variant="outline" onclick={() => startDelete(s)}>{t('svcDelete')}</Button>
								{/if}
							</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
	{/if}

	{#if deleting}
		{@const svc = services.state.status === 'ok' ? services.state.value.find((x) => x.id === deleting) : undefined}
		<Card.Root class="border-red-300 dark:border-red-900">
			<Card.Header>
				<Card.Title><h2>{t('svcDeleteTitle', { name: svc?.name ?? deleting })}</h2></Card.Title>
				<Card.Description>{t('svcDeleteIntro')}</Card.Description>
			</Card.Header>
			<Card.Content class="space-y-3">
				<label class="flex items-center gap-2 text-sm">
					<input type="checkbox" class={checkClass} bind:checked={removeVolumes} onchange={() => { delPlan = undefined; delStarted = undefined; }} />{t('svcRemoveVolumes')}
				</label>
				<p class="text-xs text-muted-foreground">{t('svcRemoveVolumesHint')}</p>
				<div class="flex flex-wrap gap-2">
					<Button size="sm" variant="outline" disabled={delBusy} onclick={() => showDeletePlan(deleting!)}>{t('svcDeletePlan')}</Button>
					<Button size="sm" variant="ghost" onclick={() => (deleting = undefined)}>{t('cancel')}</Button>
				</div>
				<div aria-live="polite" class="space-y-3">
					{#if delError}<ApiGate error={delError} />{/if}
					{#if delPlan}
						<PlanBlock plan={delPlan} />
						{#if !delStarted}
							<Button size="sm" variant="destructive" disabled={delBusy || !planView(delPlan).canStart} onclick={() => requestDelete(deleting!)}>{t('svcDeleteRequest')}</Button>
						{/if}
					{/if}
					{#if delStarted}
						<Alert.Root><Alert.Description>{t('svcDeleteRequested', { id: delStarted.operation.id })}</Alert.Description></Alert.Root>
						<FollowOperation id={delStarted.operation.id} />
					{/if}
				</div>
			</Card.Content>
		</Card.Root>
	{/if}

	<Card.Root>
		<Card.Header><Card.Title><h2>{t('svcNew')}</h2></Card.Title></Card.Header>
		<Card.Content>
			<form class="space-y-4" onsubmit={preview} aria-label={t('svcNew')}>
				<div class="grid gap-4 md:grid-cols-3">
					<div class="space-y-1.5">
						<Label for="sv-swarm">{t('svcSwarm')}</Label>
						<select id="sv-swarm" class={selectClass} bind:value={form.swarm} aria-invalid={invalid.includes('swarm')}>
							<option value="">{t('choose')}</option>
							{#if swarms.state.status === 'ok'}{#each swarms.state.value as s (s.id)}<option value={s.id}>{s.name}</option>{/each}{/if}
						</select>
						{#if err('swarm', 'valSvcSwarm')}<p class="text-xs text-red-700 dark:text-red-400">{t('valSvcSwarm')}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="sv-type">{t('svcType')}</Label>
						<select id="sv-type" class={selectClass} value={form.type} onchange={(e) => setType(e.currentTarget.value as ServiceType)}>
							<option value="container">{t('svcTypeContainer')}</option>
							<option value="traefik">{t('svcTypeTraefik')}</option>
						</select>
					</div>
					<div class="space-y-1.5">
						<Label for="sv-name">{t('svcName')}</Label>
						<Input id="sv-name" bind:value={form.name} autocomplete="off" aria-invalid={invalid.includes('name')} aria-describedby="sv-name-hint" />
						<p id="sv-name-hint" class="text-xs {invalid.includes('name') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('name') ? t('valSvcName') : t('svcNameHint')}</p>
					</div>
				</div>
				<div class="grid gap-4 md:grid-cols-2">
					<div class="space-y-1.5">
						<Label for="sv-image">{t('svcImage')}</Label>
						<Input id="sv-image" class="font-mono" bind:value={form.image} autocomplete="off" placeholder={form.type === 'traefik' ? 'traefik:v3.1.2' : 'nginx:1.27'} aria-invalid={invalid.includes('image')} aria-describedby="sv-image-hint" />
						<p id="sv-image-hint" class="text-xs {invalid.includes('image') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('image') ? t('valSvcImage') : t('svcImageHint')}</p>
					</div>
					<div class="space-y-1.5">
						<Label for="sv-host">{t('svcHost')}</Label>
						<select id="sv-host" class={selectClass} bind:value={form.hostId} aria-describedby="sv-host-hint">
							<option value="">{t('svcHostAny')}</option>
							{#each hostChoices as h (h.nodeId)}<option value={h.nodeId}>{h.hostname ?? h.nodeId}</option>{/each}
						</select>
						<p id="sv-host-hint" class="text-xs {invalid.includes('volumeHost') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('volumeHost') ? t('valSvcVolumeHost') : t('svcHostHint')}</p>
					</div>
				</div>

				{#if form.type === 'container'}
					<div class="grid gap-4 md:grid-cols-4">
						<div class="space-y-1.5">
							<Label for="sv-rep">{t('svcReplicas')}</Label>
							<Input id="sv-rep" inputmode="numeric" bind:value={form.replicas} aria-invalid={invalid.includes('replicas')} />
							{#if invalid.includes('replicas')}<p class="text-xs text-red-700 dark:text-red-400">{t('valSvcReplicas')}</p>{/if}
						</div>
						<div class="space-y-1.5">
							<Label for="sv-cpu">{t('svcCpu')}</Label>
							<Input id="sv-cpu" inputmode="numeric" bind:value={form.cpuMilli} aria-invalid={invalid.includes('cpu')} />
							{#if invalid.includes('cpu')}<p class="text-xs text-red-700 dark:text-red-400">{t('valSvcCpu')}</p>{/if}
						</div>
						<div class="space-y-1.5">
							<Label for="sv-mem">{t('svcMemory')}</Label>
							<Input id="sv-mem" inputmode="numeric" bind:value={form.memoryMiB} aria-invalid={invalid.includes('memory')} />
							{#if invalid.includes('memory')}<p class="text-xs text-red-700 dark:text-red-400">{t('valSvcMemory')}</p>{/if}
						</div>
						<div class="space-y-1.5">
							<Label for="sv-restart">{t('svcRestart')}</Label>
							<select id="sv-restart" class={selectClass} bind:value={form.restart}>
								{#each ['any', 'on-failure', 'none'] as r (r)}<option value={r}>{t(restartLabel[r as RestartPolicy])}</option>{/each}
							</select>
						</div>
					</div>
					<div class="grid gap-4 md:grid-cols-2">
						<div class="space-y-1.5">
							<Label for="sv-env">{t('svcEnv')}</Label>
							<textarea id="sv-env" rows="3" class="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring" bind:value={form.env} aria-invalid={invalid.includes('env') || invalid.includes('envSecret')} aria-describedby="sv-env-hint" placeholder="TZ=Europe/Berlin"></textarea>
							<p id="sv-env-hint" class="text-xs {invalid.includes('env') || invalid.includes('envSecret') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">
								{invalid.includes('envSecret') ? t('valSvcEnvSecret') : invalid.includes('env') ? t('valSvcEnv') : t('svcEnvHint')}
							</p>
						</div>
						<div class="space-y-1.5">
							<Label for="sv-vol">{t('svcVolumes')}</Label>
							<textarea id="sv-vol" rows="3" class="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring" bind:value={form.volumes} aria-invalid={invalid.includes('volumes')} aria-describedby="sv-vol-hint" placeholder="data:/var/lib/app"></textarea>
							<p id="sv-vol-hint" class="text-xs {invalid.includes('volumes') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('volumes') ? t('valSvcVolumes') : t('svcVolumesHint')}</p>
						</div>
						<div class="space-y-1.5">
							<Label for="sv-net">{t('svcNetworks')}</Label>
							<Input id="sv-net" bind:value={form.networks} autocomplete="off" aria-invalid={invalid.includes('networks')} aria-describedby="sv-net-hint" />
							<p id="sv-net-hint" class="text-xs {invalid.includes('networks') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('networks') ? t('valSvcNetworks') : t('svcNetworksHint')}</p>
						</div>
						<div class="space-y-1.5">
							<Label for="sv-sec">{t('svcSecrets')}</Label>
							<Input id="sv-sec" bind:value={form.secrets} autocomplete="off" aria-describedby="sv-sec-hint" />
							<p id="sv-sec-hint" class="text-xs text-muted-foreground">{t('svcSecretsHint')}</p>
						</div>
					</div>
				{:else}
					<div class="grid gap-4 md:grid-cols-3">
						<div class="space-y-1.5">
							<Label for="sv-log">{t('svcLogLevel')}</Label>
							<select id="sv-log" class={selectClass} bind:value={form.logLevel}>{#each logLevels as l (l)}<option value={l}>{l}</option>{/each}</select>
						</div>
						<div class="space-y-1.5 md:col-span-2">
							<Label for="sv-mail">{t('svcAcmeEmail')}</Label>
							<Input id="sv-mail" type="email" bind:value={form.acmeEmail} autocomplete="off" aria-invalid={invalid.includes('email') || invalid.includes('tlsEmail')} aria-describedby="sv-mail-hint" placeholder="ops@example.org" />
							<p id="sv-mail-hint" class="text-xs {invalid.includes('email') || invalid.includes('tlsEmail') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">
								{invalid.includes('tlsEmail') ? t('valSvcTlsEmail') : invalid.includes('email') ? t('valSvcEmail') : t('svcAcmeEmailHint')}
							</p>
						</div>
					</div>
					<div>
						<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={form.acmeStaging} />{t('svcStaging')}</label>
						<p class="mt-1 text-xs text-muted-foreground">{t('svcStagingHint')}</p>
					</div>
					<fieldset class="space-y-2">
						<legend class="text-sm font-medium">{t('svcRoutes')}</legend>
						<p class="text-xs {invalid.includes('routes') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('routes') ? t('valSvcRoutes') : t('svcRoutesHint')}</p>
						{#each form.routes as r, i (i)}
							<div class="grid items-end gap-2 md:grid-cols-[1fr_1fr_auto_auto]">
								<div class="space-y-1">
									<Label for="sv-rh-{i}" class="text-xs">{t('svcRouteHost')}</Label>
									<Input id="sv-rh-{i}" bind:value={r.host} autocomplete="off" placeholder="app.example.org" />
								</div>
								<div class="space-y-1">
									<Label for="sv-rb-{i}" class="text-xs">{t('svcRouteBackend')}</Label>
									<Input id="sv-rb-{i}" class="font-mono" bind:value={r.backend} autocomplete="off" placeholder="http://web-app:8080" />
								</div>
								<label class="flex h-9 items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={r.tls} />{t('svcRouteTls')}</label>
								<Button type="button" size="sm" variant="ghost" disabled={form.routes.length === 1} onclick={() => (form.routes = form.routes.filter((_, j) => j !== i))}>{t('svcRouteRemove')}</Button>
							</div>
						{/each}
						<Button type="button" size="sm" variant="outline" onclick={() => (form.routes = [...form.routes, { host: '', backend: '', tls: true }])}>{t('svcRouteAdd')}</Button>
					</fieldset>
				{/if}

				<Button type="submit" disabled={planBusy}>{t('svcPreview')}</Button>
			</form>

			<div aria-live="polite" class="mt-4 space-y-3">
				{#if planError}<ApiGate error={planError} />{/if}
				{#if shown}
					<section class="space-y-3 rounded-md border p-3" aria-label={t('svcPlanTitle')}>
						<h3 class="font-medium">{t('svcPlanTitle')}</h3>
						<PlanBlock plan={shown.plan} />
						{#if stale}
							<Alert.Root><Alert.Description>{t('svcPlanStale')}</Alert.Description></Alert.Root>
						{:else if !started}
							<Button disabled={requestBusy || !planView(shown.plan).canStart} onclick={request}>{t('svcRequest')}</Button>
						{/if}
						{#if requestError}<ApiGate error={requestError} />{/if}
					</section>
				{/if}
				{#if started}
					<Alert.Root><Alert.Description>{t('svcRequested', { id: started.operation.id })}</Alert.Description></Alert.Root>
					<FollowOperation id={started.operation.id} />
				{/if}
			</div>
		</Card.Content>
	</Card.Root>
</div>
