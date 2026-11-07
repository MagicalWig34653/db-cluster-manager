<script lang="ts">
	import { patchDays, patchModes, patchReboots, type PatchPolicy } from '../../api';
	import type { MessageKey } from '../../i18n';
	import { t } from '../../locale.svelte';
	import { checkClass, selectClass } from './fieldClass';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';

	/** Fields of one patch policy. `prefix` keeps element ids unique when several forms are on the page. */
	let { policy = $bindable(), errors, prefix }: { policy: PatchPolicy; errors: MessageKey[]; prefix: string } = $props();
	const windows = $derived(policy.windows ?? []);
	const addWindow = () => { policy.windows = [...windows, { start: '03:00', durationMinutes: 120 }]; };
	const removeWindow = (i: number) => { policy.windows = windows.filter((_, j) => j !== i); };
	const toggleDay = (i: number, day: (typeof patchDays)[number], on: boolean) => {
		const days = windows[i].days ?? [];
		windows[i].days = on ? [...days, day] : days.filter((d) => d !== day);
	};
</script>

<div class="grid gap-4 md:grid-cols-2">
	<div class="space-y-1.5">
		<Label for="{prefix}-mode">{t('patchPolicyMode')}</Label>
		<select id="{prefix}-mode" class={selectClass} bind:value={policy.mode}>
			{#each patchModes as m (m)}<option value={m}>{t(`patchMode_${m}`)}</option>{/each}
		</select>
	</div>
	<div class="space-y-1.5">
		<Label for="{prefix}-reboot">{t('patchPolicyReboot')}</Label>
		<select id="{prefix}-reboot" class={selectClass} bind:value={policy.reboot} aria-invalid={errors.includes('valPatchRebootWindow')}>
			{#each patchReboots as r (r)}<option value={r}>{t(`patchReboot_${r}`)}</option>{/each}
		</select>
	</div>
	<div class="space-y-1.5">
		<Label for="{prefix}-runtime">{t('patchPolicyContainerRuntime')}</Label>
		<select id="{prefix}-runtime" class={selectClass} bind:value={policy.containerRuntime} aria-describedby="{prefix}-runtime-note">
			{#each patchModes as m (m)}<option value={m}>{t(`patchMode_${m}`)}</option>{/each}
		</select>
		<p id="{prefix}-runtime-note" class="text-xs {policy.containerRuntime === 'approval_required' ? 'text-amber-700 dark:text-amber-400' : 'text-muted-foreground'}">
			{policy.containerRuntime === 'approval_required' ? t('patchPolicyContainerRuntimeWarn') : t('patchPolicyContainerRuntimeDefault')}
		</p>
	</div>
	<div class="space-y-1.5">
		<Label for="{prefix}-every">{t('patchPolicyReportEvery')}</Label>
		<Input id="{prefix}-every" type="number" min="0" max="10080" bind:value={policy.reportEveryMinutes} aria-invalid={errors.includes('valPatchReportEvery')} />
	</div>
</div>
<label class="mt-3 flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={policy.securityOnly} />{t('patchPolicySecurityOnly')}</label>

<fieldset class="mt-4 space-y-3">
	<legend class="text-sm font-medium">{t('patchPolicyWindows')}</legend>
	{#if !windows.length}<p class="text-sm text-muted-foreground">{t('patchPolicyNoWindows')}</p>{/if}
	{#each windows as w, i (i)}
		<div class="grid gap-3 rounded-md border p-3 md:grid-cols-3">
			<fieldset class="md:col-span-3">
				<legend class="text-xs text-muted-foreground">{t('patchWindowDays')}</legend>
				<div class="flex flex-wrap gap-3">
					{#each patchDays as d (d)}
						<label class="flex items-center gap-1 text-sm"><input type="checkbox" class={checkClass} checked={w.days?.includes(d) ?? false} onchange={(e) => toggleDay(i, d, e.currentTarget.checked)} />{t(`patchDay_${d}`)}</label>
					{/each}
				</div>
			</fieldset>
			<div class="space-y-1.5">
				<Label for="{prefix}-w{i}-start">{t('patchWindowStart')}</Label>
				<Input id="{prefix}-w{i}-start" class="font-mono" bind:value={w.start} autocomplete="off" aria-invalid={errors.includes('valPatchWindowStart')} />
			</div>
			<div class="space-y-1.5">
				<Label for="{prefix}-w{i}-dur">{t('patchWindowDuration')}</Label>
				<Input id="{prefix}-w{i}-dur" type="number" min="1" max="1440" bind:value={w.durationMinutes} aria-invalid={errors.includes('valPatchWindowDuration')} />
			</div>
			<div class="space-y-1.5">
				<Label for="{prefix}-w{i}-zone">{t('patchWindowZone')}</Label>
				<Input id="{prefix}-w{i}-zone" bind:value={w.zone} autocomplete="off" placeholder="Europe/Berlin" />
			</div>
			<div class="md:col-span-3"><Button type="button" variant="outline" size="sm" onclick={() => removeWindow(i)}>{t('patchWindowRemove', { n: i + 1 })}</Button></div>
		</div>
	{/each}
	<Button type="button" variant="outline" size="sm" onclick={addWindow}>{t('patchWindowAdd')}</Button>
</fieldset>
