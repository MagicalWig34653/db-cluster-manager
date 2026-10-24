<script lang="ts">
	import { api, asApiError, type ApiError, type VulnPollReport } from '../../../lib/api';
	import {
		countByPriority,
		coverageIsPartial,
		defaultFilter,
		feedErrorLabel,
		feedHealth,
		filterFindings,
		findingOrder,
		manualStatuses,
		priorityTone,
		serviceKindLabel,
		severityLabel,
		severityTone,
		statusLabel,
		toneText
	} from '../../../lib/vuln';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { dateTime } from '../../../lib/format';
	import { selectClass } from '../../../lib/components/app/fieldClass';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ErrorNotice from '../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const findings = load(api.vulnFindings);
	const feeds = load(api.vulnFeeds);
	let filter = $state(defaultFilter());
	let polling = $state(false);
	let report = $state<VulnPollReport | undefined>();
	let pollError = $state<ApiError | undefined>();

	const all = $derived(findings.state.status === 'ok' ? findings.state.value : []);
	const shown = $derived(findingOrder(filterFindings(all, filter)));
	const counts = $derived(countByPriority(all));

	async function poll() {
		polling = true;
		pollError = undefined;
		try {
			report = await api.pollVuln();
			findings.refresh();
			feeds.refresh();
		} catch (x) {
			pollError = asApiError(x);
		} finally {
			polling = false;
		}
	}
	const feedText = (f: { lastError?: string }) => (f.lastError ? t('vulnFeedError', { error: t(feedErrorLabel[f.lastError] ?? 'vulnFeedErrTransport') }) : '');
</script>

<PageHeader title={t('vulnTitle')} intro={t('vulnIntro')}>
	{#snippet actions()}
		<Button variant="outline" href="/vulnerabilities/audit">{t('vulnAudit')}</Button>
		<Button variant="outline" href="/vulnerabilities/settings">{t('vulnSettingsLink')}</Button>
		<Button onclick={poll} disabled={polling}>{polling ? t('vulnPolling') : t('vulnPollNow')}</Button>
	{/snippet}
</PageHeader>

<div class="space-y-6">
	<Alert.Root><Alert.Description>{t('vulnLimitNote')}</Alert.Description></Alert.Root>
	{#if pollError}<ErrorNotice error={pollError} />{/if}
	{#if report}
		<Alert.Root role="status">
			<Alert.Description>
				{t('vulnPollResult', { open: report.findings, new: report.newFindings.length, resolved: report.resolved.length })}
				{#if report.unknown.length > 0}
					<br />{t('vulnUnknownCoverage', { count: report.unknown.length, images: report.unknown.join(', ') })}
				{/if}
			</Alert.Description>
		</Alert.Root>
	{/if}

	<section class="space-y-3" aria-labelledby="vuln-findings">
		<h2 id="vuln-findings" class="sr-only">{t('vulnTitle')}</h2>
		{#if findings.state.status === 'loading'}
			<Loading />
		{:else if findings.state.status === 'error'}
			<ErrorNotice error={findings.state.error} retry={findings.reload} />
		{:else}
			<p class="text-sm text-muted-foreground">{t('vulnSummary', { p1: counts.P1, p2: counts.P2, p3: counts.P3, p4: counts.P4 })}</p>
			<div class="grid gap-3 sm:grid-cols-3">
				<div class="space-y-1.5">
					<Label for="vf-status">{t('vulnFilterStatus')}</Label>
					<select id="vf-status" class={selectClass} bind:value={filter.status}>
						<option value="open">{t('vulnStatusOpen')}</option>
						<option value="all">{t('vulnStatusAll')}</option>
						{#each [...manualStatuses, 'fixed' as const] as s (s)}<option value={s}>{t(statusLabel[s])}</option>{/each}
					</select>
				</div>
				<div class="space-y-1.5">
					<Label for="vf-prio">{t('vulnFilterPriority')}</Label>
					<select id="vf-prio" class={selectClass} bind:value={filter.priority}>
						<option value="all">{t('vulnAllPriorities')}</option>
						{#each ['P1', 'P2', 'P3', 'P4'] as p (p)}<option value={p}>{p}</option>{/each}
					</select>
				</div>
				<div class="space-y-1.5">
					<Label for="vf-q">{t('vulnSearch')}</Label>
					<Input id="vf-q" type="search" bind:value={filter.query} />
				</div>
			</div>
			{#if all.length === 0}
				<p class="text-sm text-muted-foreground">{t('vulnEmpty')}</p>
			{:else if shown.length === 0}
				<p class="text-sm text-muted-foreground">{t('vulnEmptyFiltered')}</p>
			{:else}
				<div class="rounded-lg border">
					<Table.Root>
						<Table.Header>
							<Table.Row>
								<Table.Head>{t('vulnColPriority')}</Table.Head>
								<Table.Head>{t('vulnColAdvisory')}</Table.Head>
								<Table.Head>{t('vulnColComponent')}</Table.Head>
								<Table.Head>{t('vulnColServices')}</Table.Head>
								<Table.Head>{t('vulnColSeverity')}</Table.Head>
								<Table.Head>{t('vulnColStatus')}</Table.Head>
								<Table.Head>{t('vulnColSeen')}</Table.Head>
							</Table.Row>
						</Table.Header>
						<Table.Body>
							{#each shown as f (f.id)}
								<Table.Row>
									<Table.Cell class="font-semibold {toneText[priorityTone(f.priority)]}">{f.priority}</Table.Cell>
									<Table.Cell class="font-medium">
										<a class="underline-offset-4 hover:underline focus-visible:underline" href="/vulnerabilities/{encodeURIComponent(f.id)}">{f.advisoryId}</a>
										{#if f.kev}<span class="ml-1 text-xs {toneText.bad}">{t('vulnKev')}</span>{/if}
									</Table.Cell>
									<Table.Cell>
										<span class="font-mono text-xs">{f.component.name} {f.component.version}</span>
										{#if coverageIsPartial(f)}<span class="block text-xs text-muted-foreground">{f.coverage === 'unknown' ? t('vulnCovUnknown') : t('vulnCovReference')}</span>{/if}
									</Table.Cell>
									<Table.Cell class="text-xs">
										{f.services.join(', ')}
										<span class="block text-muted-foreground">{t(serviceKindLabel[f.serviceKind])}</span>
									</Table.Cell>
									<Table.Cell class={toneText[severityTone(f.severity)]}>{t(severityLabel[f.severity])}</Table.Cell>
									<Table.Cell>{t(statusLabel[f.status])}</Table.Cell>
									<Table.Cell class="whitespace-normal text-xs text-muted-foreground">{dateTime(f.firstSeen, locale.value)}</Table.Cell>
								</Table.Row>
							{/each}
						</Table.Body>
					</Table.Root>
				</div>
			{/if}
		{/if}
	</section>

	<section class="space-y-2" aria-labelledby="vuln-feeds">
		<h2 id="vuln-feeds" class="text-lg font-semibold">{t('vulnFeeds')}</h2>
		{#if feeds.state.status === 'loading'}
			<Loading />
		{:else if feeds.state.status === 'error'}
			<ErrorNotice error={feeds.state.error} retry={feeds.reload} />
		{:else if feeds.state.value.length === 0}
			<p class="text-sm text-muted-foreground">{t('vulnFeedsEmpty')}</p>
		{:else}
			<div class="rounded-lg border">
				<Table.Root>
					<Table.Header>
						<Table.Row>
							<Table.Head>{t('vulnColFeed')}</Table.Head>
							<Table.Head>{t('vulnColFeedState')}</Table.Head>
							<Table.Head>{t('vulnColLastSuccess')}</Table.Head>
							<Table.Head>{t('vulnColAdvisories')}</Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each feeds.state.value as f (f.name)}
							{@const h = feedHealth(f)}
							<Table.Row>
								<Table.Cell class="font-mono text-xs">{f.name}</Table.Cell>
								<Table.Cell class={h === 'error' ? toneText.bad : h === 'stale' ? toneText.warn : h === 'ok' ? toneText.ok : ''}>
									{#if h === 'error'}{feedText(f)}{:else if h === 'stale'}{t('vulnFeedStale')}{:else if h === 'never'}{t('vulnFeedNever')}{:else}{t('vulnFeedOk')}{/if}
								</Table.Cell>
								<Table.Cell><Freshness at={f.lastSuccess} /></Table.Cell>
								<Table.Cell>{f.advisories ?? 0}</Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			</div>
		{/if}
	</section>
</div>
