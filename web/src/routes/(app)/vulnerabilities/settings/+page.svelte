<script lang="ts">
	import { api, asApiError, vulnModes, vulnProviderKinds, type ApiError, type VulnProvider } from '../../../../lib/api';
	import {
		autoKinds,
		autoSeverities,
		buildProviderInput,
		consentRequest,
		emptyProviderForm,
		isCliKind,
		modeKey,
		modelChoices,
		needsApiKey,
		needsAutoAck,
		policyFromForm,
		policyToForm,
		providerKindLabel,
		providerUsable,
		severityLabel,
		validatePolicyForm,
		validateProviderForm,
		vulnErrorKey,
		type PolicyForm,
		type PolicyFormError,
		type ProviderFormError
	} from '../../../../lib/vuln';
	import { load } from '../../../../lib/load.svelte';
	import { t } from '../../../../lib/locale.svelte';
	import { errorMessage } from '../../../../lib/format';
	import type { MessageKey } from '../../../../lib/i18n';
	import { checkClass, selectClass } from '../../../../lib/components/app/fieldClass';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import * as Table from '../../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
	import ErrorNotice from '../../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';

	const policy = load(api.vulnPolicy);
	const providers = load(api.vulnProviders);

	let form = $state<PolicyForm | undefined>();
	let loadedVersion = $state(-1);
	let policyInvalid = $state<PolicyFormError[]>([]);
	let policyMsg = $state<string | undefined>();
	let policyErr = $state<string | undefined>();

	// Take the stored policy into the form once per version.
	$effect(() => {
		if (policy.state.status === 'ok' && policy.state.value.version !== loadedVersion) {
			form = policyToForm(policy.state.value.policy);
			loadedVersion = policy.state.value.version;
		}
	});

	const stored = $derived(policy.state.status === 'ok' ? policy.state.value.policy : undefined);
	const provs = $derived(providers.state.status === 'ok' ? providers.state.value : []);
	const usable = $derived(provs.filter(providerUsable));
	const selected = $derived(provs.find((p) => p.id === form?.provider));

	const explain = (e: ApiError) => {
		const k = vulnErrorKey(e);
		if (k) return t(k);
		const m = errorMessage(e);
		return t(m.key, m.params);
	};
	const policyMsgKey: Record<PolicyFormError, MessageKey> = {
		services: 'vulnValServices',
		severities: 'vulnValSeverities',
		kinds: 'vulnValKinds',
		maxPerDay: 'vulnValMax',
		mitigationDays: 'vulnValMitigation',
		providerModel: 'vulnValProviderModel',
		autoAck: 'vulnValAck'
	};
	const toggle = (list: string[], v: string, on: boolean) => (on ? [...new Set([...list, v])] : list.filter((x) => x !== v));
	const modeHelp: Record<string, MessageKey> = { notify_only: 'vulnModeNotifyOnlyHelp', approval_required: 'vulnModeApprovalHelp', auto_for_policy: 'vulnModeAutoHelp' };

	async function savePolicy(e: SubmitEvent) {
		e.preventDefault();
		if (!form || policy.state.status !== 'ok') return;
		policyMsg = policyErr = undefined;
		policyInvalid = validatePolicyForm(form);
		if (policyInvalid.length > 0) return;
		try {
			await api.saveVulnPolicy(policyFromForm(form), policy.state.value.version);
			policyMsg = t('vulnPolicySaved');
			policy.reload();
		} catch (x) {
			policyErr = explain(asApiError(x));
		}
	}

	// Provider creation.
	let pform = $state(emptyProviderForm());
	let pInvalid = $state<ProviderFormError[]>([]);
	let pErr = $state<string | undefined>();
	let pMsg = $state<string | undefined>();
	let pBusy = $state(false);
	const pMsgKey: Record<ProviderFormError, MessageKey> = {
		id: 'vulnValPId',
		name: 'vulnValPName',
		baseUrl: 'vulnValPBaseUrl',
		apiKey: 'vulnValPApiKey',
		binary: 'vulnValPBinary',
		experimental: 'vulnValPExperimental',
		defaultModel: 'vulnValPDefaultModel',
		limits: 'vulnValPLimits'
	};
	async function addProvider(e: SubmitEvent) {
		e.preventDefault();
		pMsg = pErr = undefined;
		pInvalid = validateProviderForm(pform);
		if (pInvalid.length > 0) return;
		pBusy = true;
		try {
			const created = await api.createVulnProvider(buildProviderInput(pform));
			pMsg = t('vulnProviderAdded', { name: created.name });
			pform = emptyProviderForm(); // the key does not stay in the page
			consentFor = created.id;
			providers.reload();
		} catch (x) {
			pErr = explain(asApiError(x));
		} finally {
			pBusy = false;
		}
	}

	// Consent step.
	let consentFor = $state<string | undefined>();
	let consentAck = $state(false);
	let consentErr = $state<string | undefined>();
	let consentMsg = $state<string | undefined>();
	const consentProvider = $derived(provs.find((p) => p.id === consentFor));
	function openConsent(p: VulnProvider) {
		consentFor = p.id;
		consentAck = false;
		consentErr = consentMsg = undefined;
	}
	async function setConsent(p: VulnProvider, grant: boolean) {
		consentErr = consentMsg = undefined;
		try {
			if (grant) {
				const r = consentRequest(p, consentAck);
				if (!r) return;
				await api.setVulnProviderConsent(p.id, true, r.statement);
				consentMsg = t('vulnConsentDone');
			} else {
				await api.setVulnProviderConsent(p.id, false);
				consentMsg = t('vulnConsentRevoked');
			}
			consentAck = false;
			providers.reload();
		} catch (x) {
			consentErr = explain(asApiError(x));
		}
	}
</script>

<PageHeader title={t('vulnSettingsTitle')} intro={t('vulnSettingsIntro')}>
	{#snippet actions()}<Button variant="outline" href="/vulnerabilities">{t('vulnBack')}</Button>{/snippet}
</PageHeader>

<div class="space-y-10">
	<section class="space-y-4" aria-labelledby="vs-policy">
		<h2 id="vs-policy" class="text-lg font-semibold">{t('vulnPolicyTitle')}</h2>
		{#if policy.state.status === 'loading'}
			<Loading />
		{:else if policy.state.status === 'error'}
			<ErrorNotice error={policy.state.error} retry={policy.reload} />
		{:else if form && stored}
			<form class="max-w-2xl space-y-4" onsubmit={savePolicy} novalidate>
				<fieldset class="space-y-2">
					<legend class="sr-only">{t('vulnPolicyTitle')}</legend>
					{#each vulnModes as m (m)}
						<label class="flex items-start gap-2 rounded-md border p-3 {form.mode === m ? 'border-primary' : ''}">
							<input type="radio" name="mode" value={m} class="{checkClass} mt-1" bind:group={form.mode} />
							<span>
								<span class="font-medium">{t(modeKey(m))}</span>
								<span class="block text-sm text-muted-foreground">{t(modeHelp[m])}</span>
							</span>
						</label>
					{/each}
				</fieldset>

				{#if form.mode === 'auto_for_policy'}
					<Alert.Root variant="destructive" role="alert">
						<Alert.Title>{t('vulnAutoWarnTitle')}</Alert.Title>
						<Alert.Description>{t('vulnAutoWarn')}</Alert.Description>
					</Alert.Root>
					<fieldset class="space-y-1.5">
						<legend class="text-sm font-medium">{t('vulnAutoSeverities')}</legend>
						<div class="flex flex-wrap gap-4">
							{#each autoSeverities as s (s)}
								<label class="flex items-center gap-2 text-sm">
									<input type="checkbox" class={checkClass} checked={form.severities.includes(s)} onchange={(e) => form && (form.severities = toggle(form.severities, s, e.currentTarget.checked))} />
									{t(severityLabel[s])}
								</label>
							{/each}
						</div>
						{#if policyInvalid.includes('severities')}<p class="text-sm text-destructive" role="alert">{t('vulnValSeverities')}</p>{/if}
					</fieldset>
					<fieldset class="space-y-1.5">
						<legend class="text-sm font-medium">{t('vulnAutoKinds')}</legend>
						<div class="flex flex-wrap gap-4">
							{#each autoKinds as k (k)}
								<label class="flex items-center gap-2 text-sm">
									<input type="checkbox" class={checkClass} checked={form.kinds.includes(k)} onchange={(e) => form && (form.kinds = toggle(form.kinds, k, e.currentTarget.checked))} />
									{k === 'patched_rebuild' ? t('vulnKindPatchedRebuild') : t('vulnKindMitigation')}
								</label>
							{/each}
						</div>
						{#if policyInvalid.includes('kinds')}<p class="text-sm text-destructive" role="alert">{t('vulnValKinds')}</p>{/if}
					</fieldset>
					<div class="space-y-1.5">
						<Label for="vs-services">{t('vulnAutoServices')}</Label>
						<textarea id="vs-services" rows="3" class="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-sm shadow-xs focus-visible:ring-2 focus-visible:ring-ring" bind:value={form.services}></textarea>
						<p class="text-xs text-muted-foreground">{t('vulnAutoServicesHint')}</p>
						{#if policyInvalid.includes('services')}<p class="text-sm text-destructive" role="alert">{t('vulnValServices')}</p>{/if}
					</div>
					<div class="max-w-xs space-y-1.5">
						<Label for="vs-max">{t('vulnAutoMax')}</Label>
						<Input id="vs-max" inputmode="numeric" bind:value={form.maxPerDay} />
						{#if policyInvalid.includes('maxPerDay')}<p class="text-sm text-destructive" role="alert">{t('vulnValMax')}</p>{/if}
					</div>
					{#if needsAutoAck(stored, form)}
						<label class="flex items-start gap-2 text-sm">
							<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={form.autoAck} />
							<span>{t('vulnAutoAck')}</span>
						</label>
						{#if policyInvalid.includes('autoAck')}<p class="text-sm text-destructive" role="alert">{t('vulnValAck')}</p>{/if}
					{:else if stored.autoOptInBy}
						<p class="text-sm text-muted-foreground">{t('vulnAutoOptIn', { by: stored.autoOptInBy })}</p>
					{/if}
				{/if}

				<div class="space-y-1.5">
					<Label for="vs-provider">{t('vulnPolicyProvider')}</Label>
					<select
						id="vs-provider"
						class={selectClass}
						value={form.provider}
						onchange={(e) => {
							if (!form) return;
							form.provider = e.currentTarget.value;
							form.model = modelChoices(provs.find((p) => p.id === form!.provider))[0] ?? '';
						}}
					>
						<option value="">{t('vulnPolicyNoProvider')}</option>
						{#each usable as p (p.id)}<option value={p.id}>{p.name}</option>{/each}
					</select>
					<p class="text-xs text-muted-foreground">{t('vulnPolicyProviderHint')}</p>
				</div>
				{#if form.provider !== ''}
					<div class="space-y-1.5">
						<Label for="vs-model">{t('vulnPolicyModel')}</Label>
						{#if modelChoices(selected).length > 0}
							<select id="vs-model" class={selectClass} bind:value={form.model}>
								{#each modelChoices(selected) as m (m)}<option value={m}>{m}</option>{/each}
							</select>
						{:else}
							<Input id="vs-model" bind:value={form.model} />
						{/if}
					</div>
				{/if}
				{#if policyInvalid.includes('providerModel')}<p class="text-sm text-destructive" role="alert">{t('vulnValProviderModel')}</p>{/if}

				<div class="max-w-xs space-y-1.5">
					<Label for="vs-mit">{t('vulnMitigationDays')}</Label>
					<Input id="vs-mit" inputmode="numeric" bind:value={form.mitigationDays} />
					{#if policyInvalid.includes('mitigationDays')}<p class="text-sm text-destructive" role="alert">{t('vulnValMitigation')}</p>{/if}
				</div>
				<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={form.retainPrompts} />{t('vulnRetainPrompts')}</label>
				<label class="flex items-center gap-2 text-sm font-medium"><input type="checkbox" class={checkClass} bind:checked={form.killSwitch} />{t('vulnKillSwitch')}</label>

				<div class="flex flex-wrap items-center gap-3">
					<Button type="submit">{t('vulnSavePolicy')}</Button>
					{#if policyMsg}<span class="text-sm" role="status">{policyMsg}</span>{/if}
				</div>
				{#if policyErr}<p class="text-sm text-destructive" role="alert">{policyErr}</p>{/if}
				{#if policyInvalid.length > 0}
					<ul class="list-disc pl-5 text-sm text-destructive" role="alert">{#each policyInvalid as k (k)}<li>{t(policyMsgKey[k])}</li>{/each}</ul>
				{/if}
			</form>
		{/if}
	</section>

	<section class="space-y-4" aria-labelledby="vs-providers">
		<h2 id="vs-providers" class="text-lg font-semibold">{t('vulnProvidersTitle')}</h2>
		<p class="text-sm text-muted-foreground">{t('vulnProvidersIntro')}</p>
		{#if providers.state.status === 'loading'}
			<Loading />
		{:else if providers.state.status === 'error'}
			<ErrorNotice error={providers.state.error} retry={providers.reload} />
		{:else if provs.length === 0}
			<p class="text-sm text-muted-foreground">{t('vulnProvidersEmpty')}</p>
		{:else}
			<div class="rounded-lg border">
				<Table.Root>
					<Table.Header>
						<Table.Row>
							<Table.Head>{t('vulnColProvider')}</Table.Head>
							<Table.Head>{t('vulnColKind')}</Table.Head>
							<Table.Head>{t('vulnColData')}</Table.Head>
							<Table.Head>{t('vulnColConsent')}</Table.Head>
							<Table.Head>{t('vulnColUsage')}</Table.Head>
							<Table.Head><span class="sr-only">{t('vulnOpenConsent')}</span></Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each provs as p (p.id)}
							<Table.Row>
								<Table.Cell class="font-medium">{p.name}<span class="block font-mono text-xs text-muted-foreground">{p.id}</span></Table.Cell>
								<Table.Cell class="text-xs">{t(providerKindLabel[p.kind])}<span class="block text-muted-foreground">{p.credentialSet ? t('vulnKeySet') : isCliKind(p.kind) ? '' : t('vulnKeyMissing')}</span></Table.Cell>
								<Table.Cell>{p.local ? t('vulnDataLocal') : t('vulnDataRemote')}</Table.Cell>
								<Table.Cell>{p.consent?.by ? t('vulnConsentGranted', { by: p.consent.by }) : t('vulnConsentNone')}</Table.Cell>
								<Table.Cell class="text-xs">{t('vulnUsage', { requests: p.usage?.requests ?? 0, tokens: p.usage?.tokens ?? 0 })}</Table.Cell>
								<Table.Cell><Button size="sm" variant="outline" onclick={() => openConsent(p)}>{t('vulnOpenConsent')}</Button></Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			</div>
		{/if}

		{#if consentProvider}
			<Card.Root>
				<Card.Header><Card.Title>{t('vulnConsentTitle', { name: consentProvider.name })}</Card.Title></Card.Header>
				<Card.Content class="space-y-3 text-sm">
					<p>{t('vulnConsentIntro')}</p>
					<blockquote class="rounded-md border-l-4 bg-muted/40 p-3">{consentProvider.egressStatement}</blockquote>
					{#if providerUsable(consentProvider)}
						<p>{t('vulnConsentGranted', { by: consentProvider.consent?.by ?? '' })}</p>
						<Button variant="outline" onclick={() => consentProvider && setConsent(consentProvider, false)}>{t('vulnConsentRevoke')}</Button>
					{:else}
						<label class="flex items-start gap-2">
							<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={consentAck} />
							<span>{t('vulnConsentAck')}</span>
						</label>
						<Button onclick={() => consentProvider && setConsent(consentProvider, true)} disabled={!consentAck}>{t('vulnConsentGrant')}</Button>
					{/if}
					{#if consentErr}<p class="text-destructive" role="alert">{consentErr}</p>{/if}
					{#if consentMsg}<p role="status">{consentMsg}</p>{/if}
					<Button variant="ghost" size="sm" onclick={() => (consentFor = undefined)}>{t('vulnCloseConsent')}</Button>
				</Card.Content>
			</Card.Root>
		{/if}

		<h3 class="pt-4 text-base font-semibold">{t('vulnAddProvider')}</h3>
		<form class="grid max-w-2xl gap-3" onsubmit={addProvider} novalidate autocomplete="off">
			<div class="grid gap-3 sm:grid-cols-2">
				<div class="space-y-1.5"><Label for="vp-id">{t('vulnPId')}</Label><Input id="vp-id" bind:value={pform.id} />{#if pInvalid.includes('id')}<p class="text-sm text-destructive" role="alert">{t('vulnValPId')}</p>{/if}</div>
				<div class="space-y-1.5"><Label for="vp-name">{t('vulnPName')}</Label><Input id="vp-name" bind:value={pform.name} />{#if pInvalid.includes('name')}<p class="text-sm text-destructive" role="alert">{t('vulnValPName')}</p>{/if}</div>
			</div>
			<div class="space-y-1.5">
				<Label for="vp-kind">{t('vulnPKind')}</Label>
				<select id="vp-kind" class={selectClass} bind:value={pform.kind}>
					{#each vulnProviderKinds as k (k)}<option value={k}>{t(providerKindLabel[k])}</option>{/each}
				</select>
			</div>
			{#if isCliKind(pform.kind)}
				<div class="space-y-1.5"><Label for="vp-bin">{t('vulnPBinary')}</Label><Input id="vp-bin" bind:value={pform.binary} />{#if pInvalid.includes('binary')}<p class="text-sm text-destructive" role="alert">{t('vulnValPBinary')}</p>{/if}</div>
				<div class="space-y-1.5"><Label for="vp-home">{t('vulnPHome')}</Label><Input id="vp-home" bind:value={pform.home} /></div>
				<label class="flex items-start gap-2 text-sm"><input type="checkbox" class="{checkClass} mt-0.5" bind:checked={pform.experimental} /><span>{t('vulnPExperimental')}</span></label>
				{#if pInvalid.includes('experimental')}<p class="text-sm text-destructive" role="alert">{t('vulnValPExperimental')}</p>{/if}
			{:else}
				<div class="space-y-1.5"><Label for="vp-url">{t('vulnPBaseUrl')}</Label><Input id="vp-url" bind:value={pform.baseUrl} />{#if pInvalid.includes('baseUrl')}<p class="text-sm text-destructive" role="alert">{t('vulnValPBaseUrl')}</p>{/if}</div>
				{#if needsApiKey(pform.kind)}
					<div class="space-y-1.5"><Label for="vp-key">{t('vulnPApiKey')}</Label><Input id="vp-key" type="password" autocomplete="off" bind:value={pform.apiKey} />{#if pInvalid.includes('apiKey')}<p class="text-sm text-destructive" role="alert">{t('vulnValPApiKey')}</p>{/if}</div>
				{/if}
			{/if}
			<div class="grid gap-3 sm:grid-cols-2">
				<div class="space-y-1.5"><Label for="vp-models">{t('vulnPModels')}</Label><Input id="vp-models" bind:value={pform.models} /></div>
				<div class="space-y-1.5"><Label for="vp-dm">{t('vulnPDefaultModel')}</Label><Input id="vp-dm" bind:value={pform.defaultModel} />{#if pInvalid.includes('defaultModel')}<p class="text-sm text-destructive" role="alert">{t('vulnValPDefaultModel')}</p>{/if}</div>
				<div class="space-y-1.5"><Label for="vp-req">{t('vulnPRequests')}</Label><Input id="vp-req" inputmode="numeric" bind:value={pform.requestsPerDay} /></div>
				<div class="space-y-1.5"><Label for="vp-tok">{t('vulnPTokens')}</Label><Input id="vp-tok" inputmode="numeric" bind:value={pform.tokensPerDay} /></div>
			</div>
			{#if pInvalid.includes('limits')}<p class="text-sm text-destructive" role="alert">{t('vulnValPLimits')}</p>{/if}
			<div><Button type="submit" disabled={pBusy}>{t('vulnCreateProvider')}</Button></div>
			{#if pErr}<p class="text-sm text-destructive" role="alert">{pErr}</p>{/if}
			{#if pMsg}<p class="text-sm" role="status">{pMsg}</p>{/if}
		</form>
	</section>
</div>
