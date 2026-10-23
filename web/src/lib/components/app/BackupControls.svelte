<script lang="ts">
	import { api, asApiError, type ApiError, type BackupSummary } from '../../api';
	import { buildPolicy, policyFormOf, validatePolicy } from '../../lifecycle';
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
	let invalid = $state<('schedule' | 'timezone')[]>([]);
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
		{#if p}{p.enabled ? p.schedule : t('policyOff')}{#if p.timezone} ({p.timezone}){/if}{:else}{summary?.policy ?? t('notReported')}{/if}
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
			<p class="text-xs text-muted-foreground">{t('policyPhysical')}</p>
			<div class="flex gap-2">
				<Button type="submit" size="sm" disabled={saving}>{t('save')}</Button>
				<Button type="button" size="sm" variant="ghost" onclick={() => (editing = false)}>{t('cancel')}</Button>
			</div>
			{#if saveError}<ApiGate error={saveError} />{/if}
		</form>
	{/if}
</div>
