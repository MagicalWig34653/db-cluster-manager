<script lang="ts">
	import { api, asApiError, type ApiError, type BackupSummary } from '../../api';
	import { buildPolicy, policyFormOf, validatePolicy, type PolicyError } from '../../lifecycle';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import { checkClass, selectClass } from './fieldClass';
	import ApiGate from './ApiGate.svelte';
	import FollowOperation from './FollowOperation.svelte';

	// Start a backup now and edit the schedule of one cluster. The manager
	// validates the schedule and enforces permissions again.
	let { cluster, name, summary, onchange }: { cluster: string; name: string; summary?: BackupSummary; onchange: () => void } = $props();

	let starting = $state(false);
	let startError = $state<ApiError | undefined>();
	let startedId = $state<string | undefined>();
	async function start() {
		starting = true;
		startError = undefined;
		startedId = undefined;
		try {
			startedId = (await api.startBackup(cluster)).id;
		} catch (e) {
			startError = asApiError(e);
		} finally {
			starting = false;
		}
	}

	let editing = $state(false);
	let form = $state(policyFormOf(undefined));
	let invalid = $state<PolicyError[]>([]);
	let saving = $state(false);
	let saved = $state(false);
	let saveError = $state<ApiError | undefined>();
	function edit() {
		// svelte-ignore state_referenced_locally
		form = policyFormOf(summary?.backupPolicy);
		invalid = [];
		saved = false;
		editing = true;
	}
	async function save(e: SubmitEvent) {
		e.preventDefault();
		invalid = validatePolicy(form);
		if (invalid.length > 0) return;
		saving = true;
		saveError = undefined;
		try {
			await api.setBackupPolicy(cluster, buildPolicy(form));
			saved = true;
			editing = false;
			onchange();
		} catch (err) {
			saveError = asApiError(err);
		} finally {
			saving = false;
		}
	}
	const p = $derived(summary?.backupPolicy);
</script>

<div class="space-y-3 text-sm">
	<h3 class="font-medium">{name}</h3>
	<p class="text-muted-foreground">
		{t('backupPolicy')}:
		{#if p}{p.enabled ? p.schedule : t('policyOff')}{#if p.timezone} ({p.timezone}){/if}{#if p.enabled}, {t('policyMethodShown', { method: t(p.method === 'logical' ? 'methodLogical' : 'methodPhysical') })}{/if}{:else}{summary?.policy ?? t('notReported')}{/if}
	</p>
	<div class="flex flex-wrap gap-2">
		<Button size="sm" disabled={starting} onclick={start}>{t('backupStart')}</Button>
		{#if !editing}<Button size="sm" variant="outline" onclick={edit}>{t('policyEdit')}</Button>{/if}
	</div>
	<p class="text-xs text-muted-foreground">{t('backupStartHint')}</p>
	<div aria-live="polite" class="space-y-2">
		{#if startError}<ApiGate error={startError} />{/if}
		{#if startedId}
			<Alert.Root><Alert.Description>{t('backupStarted', { id: startedId })}</Alert.Description></Alert.Root>
			<FollowOperation id={startedId} />
		{/if}
		{#if saved}<Alert.Root><Alert.Description>{t('policySaved')}</Alert.Description></Alert.Root>{/if}
	</div>

	{#if editing}
		<form class="space-y-3 rounded-md border p-3" onsubmit={save} aria-label={t('policyTitle', { cluster: name })}>
			<label class="flex items-center gap-2"><input type="checkbox" class={checkClass} bind:checked={form.enabled} />{t('policyEnabled')}</label>
			<div class="space-y-1.5">
				<Label for="bp-sched-{cluster}">{t('policySchedule')}</Label>
				<Input id="bp-sched-{cluster}" class="font-mono" bind:value={form.schedule} autocomplete="off" aria-invalid={invalid.includes('schedule')} aria-describedby="bp-sched-hint-{cluster}" />
				<p id="bp-sched-hint-{cluster}" class="text-xs {invalid.includes('schedule') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">
					{invalid.includes('schedule') ? t('valCron') : t('policyScheduleHint')}
				</p>
			</div>
			<div class="space-y-1.5">
				<Label for="bp-tz-{cluster}">{t('policyTimezone')}</Label>
				<Input id="bp-tz-{cluster}" bind:value={form.timezone} autocomplete="off" aria-invalid={invalid.includes('timezone')} />
				{#if invalid.includes('timezone')}<p class="text-xs text-red-700 dark:text-red-400">{t('valTimezone')}</p>{/if}
			</div>
			<div class="space-y-1.5">
				<Label for="bp-missed-{cluster}">{t('policyMissed')}</Label>
				<select id="bp-missed-{cluster}" class={selectClass} bind:value={form.missed}>
					<option value="skip">{t('policySkip')}</option>
					<option value="run-once">{t('policyRunOnce')}</option>
				</select>
			</div>
			<div class="space-y-1.5">
				<Label for="bp-method-{cluster}">{t('policyMethod')}</Label>
				<select id="bp-method-{cluster}" class={selectClass} bind:value={form.method}>
					<option value="physical">{t('policyMethodPhysical')}</option>
					<option value="logical">{t('policyMethodLogical')}</option>
				</select>
			</div>
			{#if form.method === 'logical'}
				<div class="space-y-1.5">
					<Label for="bp-dbs-{cluster}">{t('policyDatabases')}</Label>
					<Input id="bp-dbs-{cluster}" class="font-mono" bind:value={form.databases} autocomplete="off" aria-invalid={invalid.includes('databases')} aria-describedby="bp-dbs-hint-{cluster}" placeholder={t('exampleDatabases')} />
					<p id="bp-dbs-hint-{cluster}" class="text-xs {invalid.includes('databases') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('databases') ? t('valDatabases') : t('policyDatabasesHint')}</p>
				</div>
			{/if}
			<div class="space-y-2">
				<label class="flex items-center gap-2"><input type="checkbox" class={checkClass} bind:checked={form.retentionEnabled} />{t('policyRetention')}</label>
				<p class="text-xs text-muted-foreground">{t('policyRetentionHint')}</p>
				{#if form.retentionEnabled}
					<div class="grid gap-2 sm:grid-cols-3">
						<div class="space-y-1">
							<Label for="bp-kl-{cluster}" class="text-xs">{t('policyKeepLast')}</Label>
							<Input id="bp-kl-{cluster}" inputmode="numeric" bind:value={form.keepLast} aria-invalid={invalid.includes('keepLast')} />
							<p class="text-xs {invalid.includes('keepLast') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('keepLast') ? t('valKeepLast') : t('policyKeepLastUnit')}</p>
						</div>
						<div class="space-y-1">
							<Label for="bp-kd-{cluster}" class="text-xs">{t('policyKeepDaily')}</Label>
							<Input id="bp-kd-{cluster}" inputmode="numeric" bind:value={form.keepDaily} aria-invalid={invalid.includes('keepDaily')} />
							<p class="text-xs {invalid.includes('keepDaily') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('keepDaily') ? t('valKeepCount') : t('policyKeepDailyUnit')}</p>
						</div>
						<div class="space-y-1">
							<Label for="bp-kw-{cluster}" class="text-xs">{t('policyKeepWeekly')}</Label>
							<Input id="bp-kw-{cluster}" inputmode="numeric" bind:value={form.keepWeekly} aria-invalid={invalid.includes('keepWeekly')} />
							<p class="text-xs {invalid.includes('keepWeekly') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('keepWeekly') ? t('valKeepCount') : t('policyKeepWeeklyUnit')}</p>
						</div>
					</div>
				{/if}
			</div>
			<div class="flex gap-2">
				<Button type="submit" size="sm" disabled={saving}>{t('save')}</Button>
				<Button type="button" size="sm" variant="ghost" onclick={() => (editing = false)}>{t('cancel')}</Button>
			</div>
			{#if saveError}<ApiGate error={saveError} />{/if}
		</form>
	{/if}
</div>
