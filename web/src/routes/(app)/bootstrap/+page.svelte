<script lang="ts">
	import { onDestroy } from 'svelte';
	import { api, asApiError, type ApiError } from '../../../lib/api';
	import {
		bootstrapProblems, defaultBootstrapForm, draftBody, progressPercent, wipeSecrets,
		type BootstrapOS, type BootstrapView
	} from '../../../lib/bootstrap';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import { selectClass, checkClass } from '../../../lib/components/app/fieldClass';

	const form = $state(defaultBootstrapForm());
	let view = $state<BootstrapView | undefined>();
	let support = $state<BootstrapOS[]>([]);
	let busy = $state(false);
	let touched = $state(false);
	let error = $state<ApiError | undefined>();
	let typed = $state('');
	let again = $state({ password: '' });
	let timer: ReturnType<typeof setInterval> | undefined;

	const problems = $derived(bootstrapProblems(form));
	const plan = $derived(view?.plan);
	const lockdownSteps = $derived(plan?.steps.filter((s) => s.gate === 'lockdown') ?? []);
	const mainSteps = $derived(plan?.steps.filter((s) => !s.gate) ?? []);
	const k = (key: string) => t(key as MessageKey);

	$effect(() => {
		void api.bootstrapSupport().then((s) => (support = s)).catch(() => undefined);
	});
	onDestroy(() => timer && clearInterval(timer));

	function startPolling() {
		stopPolling();
		timer = setInterval(async () => {
			if (!view) return;
			try {
				view = await api.bootstrapGet(view.id);
				if (view.status !== 'running') stopPolling();
			} catch (e) { error = asApiError(e); stopPolling(); }
		}, 1500);
	}
	function stopPolling() { if (timer) clearInterval(timer); timer = undefined; }

	async function act(fn: () => Promise<BootstrapView>, wipe = false) {
		if (busy) return;
		busy = true; error = undefined;
		try {
			view = await fn();
			if (view.status === 'running') startPolling();
		} catch (e) { error = asApiError(e); }
		finally {
			// Never retain secrets in page state after the request.
			if (wipe) wipeSecrets(form);
			busy = false;
		}
	}
	function readKey(e: SubmitEvent) {
		e.preventDefault(); touched = true;
		if (problems.length) return;
		void act(() => api.bootstrapDraft(draftBody(form)), true);
	}
	function confirm(e: SubmitEvent) {
		e.preventDefault();
		if (!view || !typed.trim()) return;
		const id = view.id, fp = typed.trim();
		typed = '';
		void act(() => api.bootstrapFingerprint(id, fp));
	}
	function supply(e: SubmitEvent) {
		e.preventDefault();
		if (!view || !again.password) return;
		const id = view.id, password = again.password;
		again.password = '';
		void act(() => api.bootstrapCredentials(id, { password }));
	}
	function reset() { stopPolling(); view = undefined; touched = false; error = undefined; wipeSecrets(form); }
	const sevClass = { block: 'text-destructive', warn: 'text-amber-600 dark:text-amber-400', info: 'text-muted-foreground' } as const;
	const canRun = $derived(!!view && (view.status === 'approved' || view.status === 'paused' || (view.status === 'main-complete' && view.lockdownApproved)));
</script>

<PageHeader title={t('bootTitle')} intro={t('bootIntro')} />
<div class="space-y-6">
	<p class="text-sm text-muted-foreground">{t('bootSafety')}</p>

	{#if !view}
		<Card.Root>
			<Card.Header><Card.Title>{t('bootStepHost')}</Card.Title></Card.Header>
			<Card.Content>
				<form onsubmit={readKey} class="space-y-5" novalidate>
					<div class="grid gap-4 sm:grid-cols-2">
						<div class="space-y-2"><Label for="boot-id">{t('bootId')}</Label><Input id="boot-id" bind:value={form.id} autocomplete="off" /></div>
						<div class="space-y-2"><Label for="boot-address">{t('bootAddress')}</Label><Input id="boot-address" bind:value={form.address} autocomplete="off" /></div>
						<div class="space-y-2"><Label for="boot-user">{t('bootUser')}</Label><Input id="boot-user" bind:value={form.user} autocomplete="off" /></div>
						<div class="space-y-2">
							<Label for="boot-auth">{t('bootAuthKind')}</Label>
							<select id="boot-auth" class={selectClass} bind:value={form.authKind}>
								<option value="password">{t('bootAuthPassword')}</option>
								<option value="key">{t('bootAuthKey')}</option>
							</select>
						</div>
					</div>
					{#if form.authKind === 'password'}
						<div class="space-y-2"><Label for="boot-password">{t('bootPassword')}</Label><Input id="boot-password" type="password" bind:value={form.password} autocomplete="off" /></div>
					{:else}
						<div class="space-y-2"><Label for="boot-key">{t('bootPrivateKey')}</Label><textarea id="boot-key" class="{selectClass} h-32 py-2 font-mono" spellcheck="false" autocomplete="off" bind:value={form.privateKey}></textarea></div>
					{/if}

					<h2 class="pt-2 text-base font-semibold">{t('bootStepProfile')}</h2>
					<div class="grid gap-4 sm:grid-cols-2">
						<div class="space-y-2"><Label for="boot-svc">{t('bootServiceUser')}</Label><Input id="boot-svc" bind:value={form.profile.serviceUser} autocomplete="off" /></div>
						<div class="space-y-2">
							<Label for="boot-runtime">{t('bootRuntime')}</Label>
							<select id="boot-runtime" class={selectClass} bind:value={form.profile.runtime}>
								<option value="docker">{t('bootRuntimeDocker')}</option>
								<option value="none">{t('bootRuntimeNone')}</option>
							</select>
						</div>
					</div>
					<fieldset class="space-y-2">
						<legend class="sr-only">{t('bootStepProfile')}</legend>
						{#each [
							['firewall', 'bootOptFirewall'], ['unattendedUpgrades', 'bootOptUnattended'], ['dedicatedKey', 'bootOptDedicatedKey'], ['disableRootLogin', 'bootOptDisableRoot'],
							['nativeAgent', 'bootOptNative'], ['containerAgent', 'bootOptContainer'], ['patch', 'bootOptPatch'], ['patchApply', 'bootOptPatchApply'], ['restic', 'bootOptRestic']
						] as [field, label] (field)}
							<label class="flex items-start gap-2 text-sm">
								<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={form.profile[field as 'firewall']} />
								<span>{k(label)}</span>
							</label>
						{/each}
					</fieldset>
					{#if form.profile.restic}
						<div class="space-y-2"><Label for="boot-restic">{t('bootResticPaths')}</Label><textarea id="boot-restic" class="{selectClass} h-24 py-2 font-mono" spellcheck="false" bind:value={form.resticPathsText}></textarea></div>
					{/if}
					<div class="grid gap-4 sm:grid-cols-2">
						<div class="space-y-2">
							<Label for="boot-cluster">{t('bootCluster')}</Label>
							<select id="boot-cluster" class={selectClass} bind:value={form.profile.cluster.platform} onchange={() => (form.profile.cluster.role = form.profile.cluster.platform === 'none' ? undefined : 'init')}>
								<option value="none">{t('bootClusterNone')}</option>
								<option value="swarm">{t('bootClusterSwarm')}</option>
								<option value="k3s">{t('bootClusterK3s')}</option>
							</select>
						</div>
						{#if form.profile.cluster.platform !== 'none'}
							<div class="space-y-2">
								<Label for="boot-role">{t('bootClusterRole')}</Label>
								<select id="boot-role" class={selectClass} bind:value={form.profile.cluster.role}>
									<option value="init">{t('bootRoleInit')}</option>
									<option value="join-manager">{t('bootRoleJoinManager')}</option>
									<option value="join-worker">{t('bootRoleJoinWorker')}</option>
								</select>
							</div>
						{/if}
						{#if form.profile.cluster.platform !== 'none' && form.profile.cluster.role && form.profile.cluster.role !== 'init'}
							<div class="space-y-2"><Label for="boot-mgr">{t('bootManagerAddress')}</Label><Input id="boot-mgr" bind:value={form.profile.cluster.managerAddress} autocomplete="off" /></div>
							<div class="space-y-2"><Label for="boot-join">{t('bootJoinToken')}</Label><Input id="boot-join" type="password" bind:value={form.joinToken} autocomplete="off" /></div>
						{/if}
					</div>

					{#if touched && problems.length}
						<div role="alert" class="rounded border border-destructive p-3 text-sm">
							<p class="font-medium">{t('bootProblems')}</p>
							<ul class="list-disc pl-5">{#each problems as p (p)}<li>{k(p)}</li>{/each}</ul>
						</div>
					{/if}
					<Button type="submit" disabled={busy}>{t('bootReadKey')}</Button>
				</form>
			</Card.Content>
		</Card.Root>
		{#if support.length}
			<Card.Root>
				<Card.Header><Card.Title>{t('bootSupportMatrix')}</Card.Title></Card.Header>
				<Card.Content><ul class="grid gap-1 text-sm sm:grid-cols-2">{#each support as os (os.id)}<li><strong>{os.name}</strong> {os.versions.join(', ')} · {os.packageManager}</li>{/each}</ul></Card.Content>
			</Card.Root>
		{/if}
	{/if}

	{#if error}<ApiGate {error} />{/if}

	{#if view}
		<Card.Root>
			<Card.Header>
				<Card.Title>{view.id}</Card.Title>
				<Card.Description><span role="status" aria-live="polite">{t('bootStatus')}: {k('bootStatus' + view.status.charAt(0).toUpperCase() + view.status.slice(1))}</span></Card.Description>
			</Card.Header>
			{#if view.error}<Card.Content><p role="alert" class="text-sm text-destructive">{view.error}</p></Card.Content>{/if}
		</Card.Root>

		{#if view.status === 'awaiting-fingerprint' && view.hostKey}
			<Card.Root>
				<Card.Header><Card.Title>{t('bootStepKey')}</Card.Title><Card.Description>{t('bootFingerprintIntro')}</Card.Description></Card.Header>
				<Card.Content>
					<p class="text-sm">{t('bootFingerprintType')}: <code>{view.hostKey.type}</code></p>
					<p class="mb-4 text-sm">{t('bootFingerprint')}: <code class="break-all">{view.hostKey.fingerprint}</code></p>
					<form onsubmit={confirm} class="space-y-3">
						<div class="space-y-2"><Label for="boot-fp">{t('bootFingerprintInput')}</Label><Input id="boot-fp" bind:value={typed} autocomplete="off" spellcheck="false" /></div>
						<div class="flex flex-wrap gap-2">
							<Button type="submit" disabled={busy || !typed.trim()}>{t('bootConfirm')}</Button>
							<Button type="button" variant="outline" disabled={busy} onclick={() => act(() => api.bootstrapCancel(view!.id))}>{t('bootCancel')}</Button>
						</div>
					</form>
				</Card.Content>
			</Card.Root>
		{/if}

		{#if plan}
			<Card.Root>
				<Card.Header>
					<Card.Title>{t('bootStepPlan')}</Card.Title>
					<Card.Description>{t('bootPlanDigest')}: <code class="break-all">{plan.digest}</code></Card.Description>
				</Card.Header>
				<Card.Content class="space-y-4">
					{#if plan.findings.length}
						<div>
							<h3 class="text-sm font-semibold">{t('bootFindings')}</h3>
							<ul class="mt-1 space-y-1 text-sm">
								{#each plan.findings as f (f.code)}
									<li class={sevClass[f.severity]}><strong>{f.severity === 'block' ? t('bootSevBlock') : f.severity === 'warn' ? t('bootSevWarn') : t('bootSevInfo')}:</strong> {f.message}</li>
								{/each}
							</ul>
						</div>
					{/if}
					{#if plan.blocked}<p role="alert" class="text-sm font-medium text-destructive">{t('bootBlocked')}</p>{/if}
					<div>
						<h3 class="text-sm font-semibold">{t('bootSteps')}</h3>
						<ol class="mt-2 space-y-2">
							{#each mainSteps as s (s.id)}
								<li class="rounded border p-3 text-sm">
									<p class="font-medium">{s.title}</p>
									<p class="text-xs text-muted-foreground">{t('bootRunAs')} {s.runAs}{s.expect === 'done' ? ' · ' + t('bootAlreadyDone') : ''}{s.managerAction ? ' · ' + t('bootManagerAction') : ''}</p>
									{#if s.script}
										<details class="mt-2"><summary class="cursor-pointer text-xs">{t('bootScript')} <code class="break-all">{s.scriptDigest?.slice(0, 12)}</code></summary><pre class="mt-1 overflow-x-auto rounded bg-muted p-2 text-xs">{s.script}</pre></details>
									{/if}
									{#if s.check}<details class="mt-1"><summary class="cursor-pointer text-xs">{t('bootCheck')}</summary><pre class="mt-1 overflow-x-auto rounded bg-muted p-2 text-xs">{s.check}</pre></details>{/if}
									{#if s.cleanup}<details class="mt-1"><summary class="cursor-pointer text-xs">{t('bootCleanup')}</summary><pre class="mt-1 overflow-x-auto rounded bg-muted p-2 text-xs">{s.cleanup}</pre></details>{/if}
								</li>
							{/each}
						</ol>
					</div>
					{#if view.status === 'planned'}
						<p class="text-sm">{t('bootApproveHint')}</p>
						<Button type="button" disabled={busy} onclick={() => act(() => api.bootstrapApprove(view!.id, plan.digest))}>{t('bootApprove')}</Button>
					{/if}
				</Card.Content>
			</Card.Root>

			<Card.Root>
				<Card.Header><Card.Title>{t('bootStepRun')}</Card.Title></Card.Header>
				<Card.Content class="space-y-4">
					<div>
						<p class="mb-1 text-sm">{t('bootProgress')}: {progressPercent(view.progress)} %</p>
						<progress class="h-2 w-full" max="100" value={progressPercent(view.progress)} aria-label={t('bootProgress')}></progress>
					</div>
					<ul class="space-y-1 text-sm">
						{#each view.progress as s (s.id)}<li>{s.title}: <strong>{k('boot' + 'State' + s.state.charAt(0).toUpperCase() + s.state.slice(1))}</strong></li>{/each}
					</ul>
					{#if !view.credentialsHeld && !['completed', 'cancelled', 'awaiting-fingerprint'].includes(view.status)}
						<form onsubmit={supply} class="space-y-2">
							<p class="text-sm">{t('bootCredentialsGone')}</p>
							<div class="space-y-2"><Label for="boot-again">{t('bootPassword')}</Label><Input id="boot-again" type="password" bind:value={again.password} autocomplete="off" /></div>
							<Button type="submit" variant="outline" disabled={busy || !again.password}>{t('bootSupply')}</Button>
						</form>
					{/if}
					<div class="flex flex-wrap gap-2">
						{#if canRun}
							<Button type="button" disabled={busy || !view.credentialsHeld} onclick={() => act(() => api.bootstrapRun(view!.id))}>{view.status === 'paused' ? t('bootResume') : view.status === 'main-complete' ? t('bootRunLockdown') : t('bootRun')}</Button>
						{/if}
						{#if !['completed', 'cancelled', 'running'].includes(view.status)}
							<Button type="button" variant="outline" disabled={busy} onclick={() => act(() => api.bootstrapCancel(view!.id))}>{t('bootCancel')}</Button>
						{/if}
						{#if view.status === 'completed' || view.status === 'cancelled'}<Button type="button" variant="outline" onclick={reset}>{t('bootTitle')}</Button>{/if}
					</div>
					{#if view.status === 'completed'}<p role="status" class="text-sm">{t('bootDone')}</p>{/if}
				</Card.Content>
			</Card.Root>

			{#if lockdownSteps.length && ['main-complete', 'running', 'paused', 'completed'].includes(view.status) && (view.status !== 'running' || view.lockdownApproved)}
				<Card.Root>
					<Card.Header>
						<Card.Title>{t('bootStepLockdown')}</Card.Title>
						<Card.Description>{t('bootLockdownIntro')}</Card.Description>
					</Card.Header>
					<Card.Content class="space-y-3">
						{#if plan.dedicatedPublicKey}<p class="text-sm">{t('bootDedicatedPublicKey')}: <code class="break-all">{plan.dedicatedPublicKey}</code></p>{/if}
						<p class="text-sm">{t('bootLockdownDigest')}: <code class="break-all">{plan.lockdownDigest}</code></p>
						<ol class="space-y-2">
							{#each lockdownSteps as s (s.id)}
								<li class="rounded border p-3 text-sm">
									<p class="font-medium">{s.title}</p>
									<p class="text-xs text-muted-foreground">{t('bootRunAs')} {s.runAs}{s.managerAction ? ' · ' + t('bootManagerAction') : ''}</p>
									{#if s.script}<details class="mt-2"><summary class="cursor-pointer text-xs">{t('bootScript')}</summary><pre class="mt-1 overflow-x-auto rounded bg-muted p-2 text-xs">{s.script}</pre></details>{/if}
									{#if s.cleanup}<details class="mt-1"><summary class="cursor-pointer text-xs">{t('bootCleanup')}</summary><pre class="mt-1 overflow-x-auto rounded bg-muted p-2 text-xs">{s.cleanup}</pre></details>{/if}
								</li>
							{/each}
						</ol>
						{#if view.status === 'main-complete' && !view.lockdownApproved}
							<Button type="button" disabled={busy} onclick={() => act(() => api.bootstrapApproveLockdown(view!.id, plan.lockdownDigest!))}>{t('bootApproveLockdown')}</Button>
						{/if}
					</Card.Content>
				</Card.Root>
			{/if}
		{/if}
	{/if}
</div>
