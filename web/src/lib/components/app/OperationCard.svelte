<script lang="ts">
	import { api, asApiError, type ApiError, type Operation } from '../../api';
	import { dateTime, relativeAge } from '../../format';
	import { approvalView, canAbort, canApprove, canResume } from '../../operations';
	import { locale, t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import { Button } from '../ui/button/index.js';
	import { checkClass } from './fieldClass';
	import ErrorNotice from './ErrorNotice.svelte';
	import Status from './Status.svelte';

	// One operation with its reason, approval and controls. Every control is
	// enabled from what the operation view reports; the manager enforces the
	// rules again. `onchange` asks the parent to reload the operation list.
	let { op, onchange }: { op: Operation; onchange: () => void } = $props();

	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let note = $state<'requested' | 'approved' | undefined>();
	let confirmAbort = $state(false);
	let reviewed = $state(false);
	const approval = $derived(approvalView(op));

	async function run(fn: () => Promise<unknown>, done: 'requested' | 'approved') {
		busy = true;
		error = undefined;
		note = undefined;
		try {
			await fn();
			note = done;
			reviewed = false;
			onchange();
		} catch (e) {
			error = asApiError(e);
		} finally {
			busy = false;
			confirmAbort = false;
		}
	}
	// The digest sent is the one of the plan that is displayed right now.
	const approve = () => approval.kind === 'required' && run(() => api.approveOperation(op.id, approval.digest), 'approved');
</script>

<div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
	<span class="font-medium">{op.kind}</span>
	<Status kind="operation" value={op.state} />
</div>
<p class="mt-0.5 text-sm text-muted-foreground">
	{#if op.cluster}<a class="underline-offset-4 hover:underline" href="/clusters/{encodeURIComponent(op.cluster)}">{op.cluster}</a>{/if}
	{#if op.swarm}<span class="font-mono text-xs">{op.swarm}</span>{/if}
	{#if !op.cluster && !op.swarm}{t('managerLevel')}{/if}
</p>
<p class="mt-0.5 text-xs text-muted-foreground">
	{#if op.startedAt}
		{t('started')}: <time datetime={op.startedAt} title={dateTime(op.startedAt, locale.value)}>{relativeAge(op.startedAt, locale.value)}</time>
	{:else}
		{t('startUnknown')}
	{/if}
	{#if op.endedAt}
		· {t('ended')}: <time datetime={op.endedAt} title={dateTime(op.endedAt, locale.value)}>{relativeAge(op.endedAt, locale.value)}</time>
	{/if}
</p>
{#if op.reason}
	<p class="mt-1 text-sm"><span class="text-muted-foreground">{t('opReason')}:</span> {op.reason}</p>
{/if}

<div class="mt-2 space-y-2 text-sm">
	{#if approval.kind === 'required'}
		<div class="space-y-2 rounded-md border p-3">
			<p class="font-medium">{t('opApproval')}: {t('approvalWaiting')}</p>
			<p class="text-xs text-muted-foreground">{t('planDigest')}: <code class="font-mono break-all">{approval.digest}</code></p>
			<label class="flex items-start gap-2">
				<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={reviewed} disabled={!canApprove(op)} />
				<span>{t('approveConfirm', { digest: approval.digest.slice(0, 12) })}</span>
			</label>
			<Button size="sm" disabled={busy || !reviewed || !canApprove(op)} onclick={approve}>{t('approveButton')}</Button>
		</div>
	{:else if approval.kind === 'approved'}
		<p>
			<span class="text-muted-foreground">{t('opApproval')}:</span>
			{#if approval.by && approval.at}
				{t('approvalDone', { by: approval.by, at: dateTime(approval.at, locale.value) ?? approval.at })}
			{:else if approval.by}
				{t('approvalDone', { by: approval.by, at: '—' })}
			{:else}
				{t('approvalDoneUnknown')}
			{/if}
		</p>
	{/if}

	{#if canResume(op) || canAbort(op)}
		<div class="flex flex-wrap gap-2">
			{#if canResume(op)}<Button size="sm" variant="outline" disabled={busy} onclick={() => run(() => api.resumeOperation(op.id), 'requested')}>{t('resumeOp')}</Button>{/if}
			{#if canAbort(op) && !confirmAbort}<Button size="sm" variant="outline" disabled={busy} onclick={() => (confirmAbort = true)}>{t('abortOp')}</Button>{/if}
		</div>
	{/if}
	{#if confirmAbort}
		<Alert.Root variant="destructive" role="alertdialog" aria-label={t('abortOp')}>
			<Alert.Description class="space-y-2">
				<p>{t('abortConfirmBody', { kind: op.kind })}</p>
				<div class="flex gap-2">
					<Button size="sm" variant="destructive" disabled={busy} onclick={() => run(() => api.abortOperation(op.id), 'requested')}>{t('abortConfirm')}</Button>
					<Button size="sm" variant="ghost" onclick={() => (confirmAbort = false)}>{t('cancel')}</Button>
				</div>
			</Alert.Description>
		</Alert.Root>
	{/if}
	<div aria-live="polite">
		{#if error}<ErrorNotice {error} />{/if}
		{#if note}<p class="text-xs text-muted-foreground">{note === 'approved' ? t('approveDone') : t('opRequested')}</p>{/if}
	</div>
</div>
