<script lang="ts">
	import { page } from '$app/state';
	import { api, asApiError, type ApiError, type VulnProposal, type VulnStatus } from '../../../../lib/api';
	import {
		approvalBlock,
		coverageLabel,
		levelLabel,
		manualStatuses,
		modeKey,
		priorityTone,
		proposalKindLabel,
		proposalOpen,
		proposalStatusLabel,
		proposalsFor,
		serviceKindLabel,
		severityLabel,
		severityTone,
		statusInputError,
		statusLabel,
		toneText,
		vulnErrorKey
	} from '../../../../lib/vuln';
	import { load } from '../../../../lib/load.svelte';
	import { locale, t } from '../../../../lib/locale.svelte';
	import { dateTime, errorMessage } from '../../../../lib/format';
	import { selectClass, checkClass } from '../../../../lib/components/app/fieldClass';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
	import ErrorNotice from '../../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';

	const id = $derived(page.params.id ?? '');
	const finding = load(() => api.vulnFinding(page.params.id ?? ''));
	const proposals = load(api.vulnProposals);
	const policy = load(api.vulnPolicy);
	const providers = load(api.vulnProviders);
	const audit = load(api.vulnAudit);

	let status = $state<VulnStatus>('under_investigation');
	let justification = $state('');
	let statusMsg = $state<string | undefined>();
	let statusErr = $state<string | undefined>();
	let requestBusy = $state(false);
	let requestErr = $state<string | undefined>();
	// Approval acknowledgement and rejection reason per proposal.
	let acks = $state<Record<string, boolean>>({});
	let reasons = $state<Record<string, string>>({});
	let busy = $state<string | undefined>();
	let decideErr = $state<Record<string, string>>({});

	const pol = $derived(policy.state.status === 'ok' ? policy.state.value.policy : undefined);
	const aiProvider = $derived(
		pol?.provider && providers.state.status === 'ok' ? providers.state.value.find((p) => p.id === pol.provider && p.consent) : undefined
	);
	const mine = $derived(proposals.state.status === 'ok' ? proposalsFor(id, proposals.state.value) : []);
	const trail = $derived(audit.state.status === 'ok' ? audit.state.value.filter((r) => r.findingId === id).slice(-20).reverse() : []);

	const explain = (e: ApiError) => {
		const k = vulnErrorKey(e);
		if (k) return t(k);
		const m = errorMessage(e);
		return t(m.key, m.params);
	};

	async function saveStatus() {
		statusMsg = statusErr = undefined;
		const bad = statusInputError(status, justification);
		if (bad) {
			statusErr = t(bad);
			return;
		}
		try {
			await api.setVulnFindingStatus(id, status, justification.trim() || undefined);
			statusMsg = t('vulnStatusSaved');
			finding.refresh();
			audit.refresh();
		} catch (x) {
			statusErr = explain(asApiError(x));
		}
	}
	async function request(useAi: boolean) {
		requestBusy = true;
		requestErr = undefined;
		try {
			await api.proposeVulnRemediation(id, useAi);
			proposals.refresh();
			audit.refresh();
		} catch (x) {
			requestErr = explain(asApiError(x));
		} finally {
			requestBusy = false;
		}
	}
	async function decide(p: VulnProposal, approve: boolean) {
		busy = p.id;
		decideErr = { ...decideErr, [p.id]: '' };
		try {
			if (approve) await api.approveVulnProposal(p.id, p.plan.digest);
			else await api.declineVulnProposal(p.id, reasons[p.id]?.trim() || undefined);
			proposals.refresh();
			audit.refresh();
		} catch (x) {
			decideErr = { ...decideErr, [p.id]: explain(asApiError(x)) };
			proposals.refresh();
		} finally {
			busy = undefined;
		}
	}
	const level = (l: string) => t(levelLabel[l] ?? 'vulnLevelNone');
</script>

{#if finding.state.status === 'loading'}
	<Loading />
{:else if finding.state.status === 'error'}
	<ErrorNotice error={finding.state.error} retry={finding.reload} />
{:else}
	{@const f = finding.state.value}
	<PageHeader title={t('vulnFindingTitle', { id: f.advisoryId })} intro={f.basis}>
		{#snippet actions()}<Button variant="outline" href="/vulnerabilities">{t('vulnBack')}</Button>{/snippet}
	</PageHeader>

	<div class="space-y-6">
		<Card.Root>
			<Card.Content class="grid gap-x-8 gap-y-2 pt-6 text-sm sm:grid-cols-2">
				<p><span class="font-semibold {toneText[priorityTone(f.priority)]}">{f.priority}</span> · {t('vulnScore', { score: f.score })}</p>
				<p class={toneText[severityTone(f.severity)]}>{t(severityLabel[f.severity])}{#if f.cvss !== undefined} · {t('vulnCvss', { value: f.cvss })}{/if}{#if f.epss !== undefined} · {t('vulnEpss', { value: f.epss })}{/if}</p>
				{#if f.kev}<p class={toneText.bad}>{t('vulnKev')}</p>{/if}
				<p><span class="text-muted-foreground">{t('vulnColComponent')}:</span> <span class="font-mono">{f.component.name} {f.component.version}</span></p>
				<p><span class="text-muted-foreground">{t('vulnColImage')}:</span> <span class="font-mono text-xs">{f.image}</span></p>
				<p><span class="text-muted-foreground">{t('vulnColServices')}:</span> {f.services.join(', ')} ({t(serviceKindLabel[f.serviceKind])})</p>
				<p>{t(coverageLabel[f.coverage])}</p>
				<p>
					<span class="text-muted-foreground">{t('vulnFixedVersions')}:</span>
					{#if f.fixedVersions && f.fixedVersions.length > 0}{f.fixedVersions.join(', ')}{:else}{t('vulnNoFixedVersion')}{/if}
				</p>
				<p><span class="text-muted-foreground">{t('vulnSources')}:</span> {f.sources.join(', ')}</p>
				{#if f.aliases && f.aliases.length > 0}<p><span class="text-muted-foreground">{t('vulnAliases')}:</span> {f.aliases.join(', ')}</p>{/if}
				<p><span class="text-muted-foreground">{t('vulnColSeen')}:</span> {dateTime(f.firstSeen, locale.value)} · {t('vulnLastSeen')}: {dateTime(f.lastSeen, locale.value)}</p>
			</Card.Content>
		</Card.Root>

		<section class="space-y-3" aria-labelledby="vd-status">
			<h2 id="vd-status" class="text-lg font-semibold">{t('vulnStatusSection')}</h2>
			<p class="text-sm text-muted-foreground">{t('vulnStatusHelp')}</p>
			<p class="text-sm">{t(statusLabel[f.status])}{#if f.statusBy} · {f.statusBy}{/if}{#if f.justification}: {f.justification}{/if}</p>
			<div class="grid max-w-xl gap-3">
				<div class="space-y-1.5">
					<Label for="vd-st">{t('vulnColStatus')}</Label>
					<select id="vd-st" class={selectClass} bind:value={status}>
						{#each manualStatuses as s (s)}<option value={s}>{t(statusLabel[s])}</option>{/each}
					</select>
				</div>
				<div class="space-y-1.5">
					<Label for="vd-just">{t('vulnJustification')}</Label>
					<Input id="vd-just" bind:value={justification} maxlength={1024} />
				</div>
				<div><Button variant="outline" onclick={saveStatus}>{t('vulnSaveStatus')}</Button></div>
				{#if statusErr}<p class="text-sm text-destructive" role="alert">{statusErr}</p>{/if}
				{#if statusMsg}<p class="text-sm" role="status">{statusMsg}</p>{/if}
			</div>
		</section>

		<section class="space-y-3" aria-labelledby="vd-prop">
			<h2 id="vd-prop" class="text-lg font-semibold">{t('vulnProposals')}</h2>
			{#if proposals.state.status === 'loading'}
				<Loading />
			{:else if proposals.state.status === 'error'}
				<ErrorNotice error={proposals.state.error} retry={proposals.reload} />
			{:else if mine.length === 0}
				<p class="text-sm text-muted-foreground">{t('vulnProposalsEmpty')}</p>
			{/if}
			{#each mine as p (p.id)}
				{@const block = approvalBlock(p, pol)}
				<Card.Root>
					<Card.Content class="space-y-3 pt-6 text-sm">
						<p class="font-semibold">
							{t(proposalKindLabel[p.kind])} · {t(proposalStatusLabel[p.status])}
							<span class="font-normal text-muted-foreground">· {p.origin === 'ai' ? t('vulnOriginAi', { provider: p.provider ?? '', model: p.model ?? '' }) : t('vulnOriginDeterministic')} · {dateTime(p.createdAt, locale.value)}</span>
						</p>
						{#if p.targetVersion}<p>{t('vulnTarget', { version: p.targetVersion })}</p>{/if}
						{#if p.mitigation}
							<p class="font-mono text-xs">{t('vulnMitigationLine', { setting: p.mitigation.setting ?? '', value: p.mitigation.value ?? '', expires: dateTime(p.mitigation.expiresAt, locale.value) ?? '' })}</p>
						{/if}
						<div>
							<p>{t('vulnBenefit')}: <strong>{level(p.rating.benefit)}</strong> · {t('vulnRisk')}: <strong>{level(p.rating.risk)}</strong></p>
							<p class="text-xs text-muted-foreground">{p.rating.basis}. {t('vulnRatingNote')}</p>
						</div>
						{#if !p.validation.ok}
							<Alert.Root variant="destructive">
								<Alert.Description>
									{t('vulnValidationFailed')}
									<ul class="list-disc pl-5">{#each p.validation.errors ?? [] as e (e)}<li>{e}</li>{/each}</ul>
								</Alert.Description>
							</Alert.Root>
						{/if}
						{#if p.aiAssessment}
							<div class="rounded-md border p-3">
								<p class="font-medium">{t('vulnAiAssessment')}</p>
								{#if p.aiAssessment.affected}<p>{t('vulnAiAffected', { text: p.aiAssessment.affected })}</p>{/if}
								{#if p.aiAssessment.benefit}<p>{t('vulnAiBenefit', { text: p.aiAssessment.benefit })}</p>{/if}
								{#if p.aiAssessment.risk}<p>{t('vulnAiRisk', { text: p.aiAssessment.risk })}</p>{/if}
							</div>
						{/if}
						{#if p.rationale}
							<div>
								<p class="font-medium">{t('vulnRationale')}</p>
								<p class="whitespace-pre-wrap text-muted-foreground">{p.rationale}</p>
							</div>
						{/if}
						<div>
							<p class="font-medium">{t('vulnPlan')}</p>
							<p class="text-xs text-muted-foreground">{p.plan.manual ? t('vulnPlanManual') : t('vulnPlanBuild')}</p>
							<ol class="mt-1 list-decimal space-y-0.5 pl-5">
								{#each p.plan.steps as s, i (i)}<li><strong>{s.title}</strong>{#if s.detail}: {s.detail}{/if}</li>{/each}
							</ol>
							{#if p.plan.majorUpgrade}<p class="mt-1 {toneText.warn}">{t('vulnPlanMajor')}</p>{/if}
							{#each p.plan.warnings as w (w)}<p class="mt-1 {toneText.warn}">{w}</p>{/each}
							<p class="mt-1 font-mono text-xs text-muted-foreground">{t('vulnPlanDigest')}: {p.plan.digest}</p>
						</div>
						<p class="text-xs text-muted-foreground">{t('vulnDecisionReasons')}: {t(modeKey(p.decision.mode))}{#if p.decision.reasons && p.decision.reasons.length > 0} · {p.decision.reasons.join('; ')}{/if}</p>
						{#if p.execution}<p>{t('vulnExecution', { text: p.execution })}</p>{/if}
						{#if p.decidedBy}<p class="text-xs text-muted-foreground">{t('vulnDecidedBy', { status: t(proposalStatusLabel[p.status]), by: p.decidedBy })}</p>{/if}
						{#if proposalOpen(p)}
							<div class="space-y-2 border-t pt-3">
								{#if block}<p class="text-muted-foreground">{t(block)}</p>{/if}
								<label class="flex items-start gap-2">
									<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={acks[p.id]} disabled={block !== undefined} />
									<span>{t('vulnApproveAck', { digest: p.plan.digest.slice(0, 19) })}</span>
								</label>
								<div class="flex flex-wrap items-end gap-3">
									<Button onclick={() => decide(p, true)} disabled={block !== undefined || !acks[p.id] || busy === p.id}>{t('vulnApprove')}</Button>
									<div class="min-w-48 flex-1 space-y-1.5">
										<Label for="rr-{p.id}">{t('vulnRejectReason')}</Label>
										<Input id="rr-{p.id}" bind:value={reasons[p.id]} maxlength={1024} />
									</div>
									<Button variant="outline" onclick={() => decide(p, false)} disabled={busy === p.id}>{t('vulnReject')}</Button>
								</div>
								{#if decideErr[p.id]}<p class="text-destructive" role="alert">{decideErr[p.id]}</p>{/if}
							</div>
						{/if}
					</Card.Content>
				</Card.Root>
			{/each}

			<div class="space-y-2">
				<div class="flex flex-wrap gap-2">
					<Button variant="outline" onclick={() => request(false)} disabled={requestBusy}>{requestBusy ? t('vulnRequesting') : t('vulnRequestPlain')}</Button>
					<Button variant="outline" onclick={() => request(true)} disabled={requestBusy || !aiProvider}>{t('vulnRequestAi')}</Button>
				</div>
				<p class="text-xs text-muted-foreground">{aiProvider ? t('vulnRequestAiHint') : t('vulnNoAiProvider')}</p>
				{#if requestErr}<p class="text-sm text-destructive" role="alert">{requestErr}</p>{/if}
			</div>
		</section>

		<section class="space-y-2" aria-labelledby="vd-audit">
			<h2 id="vd-audit" class="text-lg font-semibold">{t('vulnAudit')}</h2>
			<p class="text-sm text-muted-foreground">{t('vulnAuditHint')}</p>
			{#if trail.length === 0}
				<p class="text-sm text-muted-foreground">{t('vulnAuditEmpty')}</p>
			{:else}
				<ul class="space-y-1 text-xs">
					{#each trail as r (r.id)}
						<li><span class="text-muted-foreground">{dateTime(r.at, locale.value)}</span> · {r.actor} · <span class="font-mono">{r.event}</span> · {r.result}{#if r.detail} · {r.detail}{/if}</li>
					{/each}
				</ul>
			{/if}
		</section>
	</div>
{/if}
