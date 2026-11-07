<script lang="ts">
	import { api, asApiError, type ApiError, type PatchGroup, type PatchHostSummary, type PatchPolicy } from '../../api';
	import { editablePatchPolicy, patchPolicyRequest, validatePatchGroup, validatePatchPolicy } from '../../patch';
	import type { MessageKey } from '../../i18n';
	import { load } from '../../load.svelte';
	import { t } from '../../locale.svelte';
	import { selectClass, checkClass } from './fieldClass';
	import { focusFirstInvalid } from './form-focus';
	import ApiGate from './ApiGate.svelte';
	import ErrorNotice from './ErrorNotice.svelte';
	import FormErrors from './FormErrors.svelte';
	import Loading from './Loading.svelte';
	import PatchPolicyFields from './PatchPolicyFields.svelte';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import * as Table from '../ui/table/index.js';

	let { hosts, onchange }: { hosts: PatchHostSummary[]; onchange: () => void } = $props();
	const defaults = load(api.patchPolicy);
	const groups = load(api.patchGroups);

	let busy = $state('');
	let error = $state<ApiError | undefined>();
	let done = $state<MessageKey | undefined>();
	let errors = $state<MessageKey[]>([]);
	let defaultDraft = $state<PatchPolicy>(editablePatchPolicy());
	let groupId = $state('');
	let groupName = $state('');
	let groupEditing = $state(false);
	let groupOwn = $state(false);
	let groupDraft = $state<PatchPolicy>(editablePatchPolicy());
	let hostId = $state('');
	let hostGroup = $state('');
	let hostOwn = $state(false);
	let hostDraft = $state<PatchPolicy>(editablePatchPolicy());
	let defaultForm = $state<HTMLFormElement | null>(null);
	let groupForm = $state<HTMLFormElement | null>(null);
	let hostForm = $state<HTMLFormElement | null>(null);

	$effect(() => { if (defaults.state.status === 'ok') defaultDraft = editablePatchPolicy(defaults.state.value); });

	/** Runs one save; `check` lists the validation errors that stop it before any request. */
	async function submit(e: SubmitEvent, key: string, form: HTMLFormElement | null, check: MessageKey[], ok: MessageKey, fn: () => Promise<unknown>) {
		e.preventDefault();
		done = undefined; error = undefined; errors = check;
		if (check.length) { await focusFirstInvalid(form); return; }
		busy = key;
		try { await fn(); done = ok; onchange(); }
		catch (x) { error = asApiError(x); }
		finally { busy = ''; }
	}
	const saveDefault = (e: SubmitEvent) =>
		submit(e, 'default', defaultForm, validatePatchPolicy(defaultDraft), 'patchPolicySaved', async () => { await api.savePatchPolicy(patchPolicyRequest(defaultDraft)); defaults.refresh(); });
	const saveGroup = (e: SubmitEvent) =>
		submit(e, 'group', groupForm, [...validatePatchGroup(groupId, groupName), ...(groupOwn ? validatePatchPolicy(groupDraft) : [])], 'patchGroupSaved', async () => {
			await api.savePatchGroup(groupId, { name: groupName, ...(groupOwn ? { policy: patchPolicyRequest(groupDraft) } : {}) });
			groups.refresh(); newGroup();
		});
	const saveHost = (e: SubmitEvent) =>
		submit(e, 'host', hostForm, hostOwn ? validatePatchPolicy(hostDraft) : [], 'patchHostSaved', async () => {
			const name = hosts.find((h) => h.host.id === hostId)?.host.name;
			await api.savePatchHost(hostId, { ...(name ? { name } : {}), ...(hostGroup ? { group: hostGroup } : {}), ...(hostOwn ? { policy: patchPolicyRequest(hostDraft) } : {}) });
			groups.refresh();
		});

	function newGroup() { groupId = ''; groupName = ''; groupEditing = false; groupOwn = false; groupDraft = editablePatchPolicy(); }
	function editGroup(g: PatchGroup) {
		groupId = g.id; groupName = g.name; groupEditing = true; groupOwn = !!g.policy; groupDraft = editablePatchPolicy(g.policy); done = undefined; error = undefined; errors = [];
	}
	async function removeGroup(g: PatchGroup) {
		if (!confirm(t('patchGroupDeleteConfirm', { id: g.id }))) return;
		busy = 'del' + g.id; done = undefined; error = undefined; errors = [];
		try { await api.deletePatchGroup(g.id); done = 'patchGroupDeleted'; groups.refresh(); onchange(); }
		catch (x) { error = asApiError(x); }
		finally { busy = ''; }
	}
	function chooseHost() {
		const h = hosts.find((x) => x.host.id === hostId)?.host;
		hostGroup = h?.group ?? ''; hostOwn = !!h?.policy; hostDraft = editablePatchPolicy(h?.policy);
	}
</script>

<section class="mt-8 space-y-6" aria-labelledby="patch-policy-title">
	<div class="space-y-1">
		<h2 id="patch-policy-title" class="text-lg font-medium">{t('patchPolicyTitle')}</h2>
		<p class="text-sm text-muted-foreground">{t('patchPolicyIntro')}</p>
	</div>
	<FormErrors {errors} />
	{#if error}<ErrorNotice {error} />{/if}
	{#if done}<p class="text-sm" role="status">{t(done)}</p>{/if}

	<form bind:this={defaultForm} onsubmit={saveDefault} aria-labelledby="patch-default-title" class="max-w-3xl space-y-3" autocomplete="off">
		<h3 id="patch-default-title" class="font-medium">{t('patchDefaultPolicy')}</h3>
		{#if defaults.state.status === 'loading'}<Loading />
		{:else if defaults.state.status === 'error'}<ApiGate error={defaults.state.error} retry={defaults.reload} />
		{:else}
			<PatchPolicyFields bind:policy={defaultDraft} {errors} prefix="pp-default" />
			<Button type="submit" disabled={busy !== ''}>{t('patchPolicySave')}</Button>
		{/if}
	</form>

	<div class="space-y-3">
		<h3 class="font-medium">{t('patchGroups')}</h3>
		{#if groups.state.status === 'loading'}<Loading />
		{:else if groups.state.status === 'error'}<ApiGate error={groups.state.error} retry={groups.reload} />
		{:else if !groups.state.value.length}<p class="text-sm text-muted-foreground">{t('patchNoGroups')}</p>
		{:else}
			<div class="overflow-x-auto">
			<Table.Root>
				<Table.Header><Table.Row>
					<Table.Head>{t('patchGroupName')}</Table.Head><Table.Head>{t('patchPolicyMode')}</Table.Head><Table.Head>{t('patchPolicyContainerRuntime')}</Table.Head><Table.Head></Table.Head>
				</Table.Row></Table.Header>
				<Table.Body>
					{#each groups.state.value as g (g.id)}
						<Table.Row>
							<Table.Cell><div class="font-medium">{g.name}</div><div class="font-mono text-xs text-muted-foreground">{g.id}</div></Table.Cell>
							<Table.Cell>{g.policy ? t(`patchMode_${g.policy.mode ?? 'report_only'}`) : t('patchDefaultPolicy')}</Table.Cell>
							<Table.Cell class={g.policy?.containerRuntime === 'approval_required' ? 'text-amber-700 dark:text-amber-400' : ''}>{g.policy ? t(`patchMode_${g.policy.containerRuntime ?? 'report_only'}`) : '—'}</Table.Cell>
							<Table.Cell class="space-x-2 whitespace-nowrap">
								<Button size="sm" variant="outline" onclick={() => editGroup(g)}>{t('patchGroupEdit')}<span class="sr-only"> {g.id}</span></Button>
								<Button size="sm" variant="outline" disabled={busy !== ''} onclick={() => removeGroup(g)}>{t('patchGroupDelete')}<span class="sr-only"> {g.id}</span></Button>
							</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
			</div>
		{/if}

		<form bind:this={groupForm} onsubmit={saveGroup} aria-label={t('patchGroupNew')} class="max-w-3xl space-y-3 rounded-md border p-3" autocomplete="off">
			<div class="grid gap-4 md:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="pg-id">{t('patchGroupId')}</Label>
					<Input id="pg-id" class="font-mono" bind:value={groupId} readonly={groupEditing} aria-invalid={errors.includes('valPatchGroupId')} aria-describedby="pg-id-hint" />
					<p id="pg-id-hint" class="text-xs text-muted-foreground">{t('patchGroupIdHint')}</p>
				</div>
				<div class="space-y-1.5">
					<Label for="pg-name">{t('patchGroupName')}</Label>
					<Input id="pg-name" bind:value={groupName} aria-invalid={errors.includes('valPatchGroupName')} />
				</div>
			</div>
			<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={groupOwn} />{t('patchGroupOwnPolicy')}</label>
			{#if groupOwn}<PatchPolicyFields bind:policy={groupDraft} {errors} prefix="pp-group" />{/if}
			<div class="flex gap-2">
				<Button type="submit" disabled={busy !== ''}>{t('patchGroupSave')}</Button>
				{#if groupEditing}<Button type="button" variant="outline" onclick={newGroup}>{t('patchGroupNew')}</Button>{/if}
			</div>
		</form>
	</div>

	<form bind:this={hostForm} onsubmit={saveHost} aria-labelledby="patch-host-title" class="max-w-3xl space-y-3" autocomplete="off">
		<h3 id="patch-host-title" class="font-medium">{t('patchHostAssign')}</h3>
		<div class="grid gap-4 md:grid-cols-2">
			<div class="space-y-1.5">
				<Label for="ph-host">{t('patchHost')}</Label>
				<select id="ph-host" class={selectClass} bind:value={hostId} onchange={chooseHost}>
					<option value="">{t('patchChooseHost')}</option>
					{#each hosts as h (h.host.id)}<option value={h.host.id}>{h.host.name}</option>{/each}
				</select>
			</div>
			<div class="space-y-1.5">
				<Label for="ph-group">{t('patchGroup')}</Label>
				<select id="ph-group" class={selectClass} bind:value={hostGroup} disabled={!hostId}>
					<option value="">{t('patchHostNoGroup')}</option>
					{#if groups.state.status === 'ok'}{#each groups.state.value as g (g.id)}<option value={g.id}>{g.name}</option>{/each}{/if}
				</select>
			</div>
		</div>
		<label class="flex items-center gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={hostOwn} disabled={!hostId} />{t('patchHostOverride')}</label>
		{#if hostOwn}<PatchPolicyFields bind:policy={hostDraft} {errors} prefix="pp-host" />{/if}
		<Button type="submit" disabled={!hostId || busy !== ''}>{t('patchHostSave')}</Button>
	</form>
</section>
