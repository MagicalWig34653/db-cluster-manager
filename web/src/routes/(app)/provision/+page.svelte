<script lang="ts">
	import { api, asApiError, type ApiError, type ProvisionPlan, type ProvisionPlanInput } from '../../../lib/api';
	import { validProvision } from '../../../lib/provision';
	import { t } from '../../../lib/locale.svelte';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import { selectClass } from '../../../lib/components/app/fieldClass';

	let form = $state<ProvisionPlanInput>({ id: '', platform: 'swarm', hosts: [{ id: 'host-1', address: '', user: '', role: 'manager' }], credentials: { 'host-1': { privateKey: '', knownHosts: '' } } });
	let plan = $state<ProvisionPlan | undefined>();
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let started = $state('');
	let touched = $state(false);

	function addHost() {
		const id = `host-${form.hosts.length + 1}`;
		form.hosts.push({ id, address: '', user: '', role: 'worker' });
		form.credentials[id] = { privateKey: '', knownHosts: '' };
	}
	function removeHost(index: number) {
		if (index === 0) return;
		const id = form.hosts[index].id;
		form.hosts.splice(index, 1);
		delete form.credentials[id];
	}
	async function preflight(e: SubmitEvent) {
		e.preventDefault(); touched = true; plan = undefined; started = '';
		if (!validProvision(form) || busy) return;
		busy = true; error = undefined;
		try { plan = await api.planProvision(form); }
		catch (e) { error = asApiError(e); }
		finally {
			// Never retain private keys in page state after the request.
			for (const c of Object.values(form.credentials)) c.privateKey = '';
			busy = false;
		}
	}
	async function execute() {
		if (!plan || plan.platform !== 'swarm' || busy) return;
		busy = true; error = undefined;
		try { started = (await api.startProvision(plan.id, plan.digest)).id; }
		catch (e) { error = asApiError(e); }
		finally { busy = false; }
	}
	function download(hostId: string, script: string) {
		const url = URL.createObjectURL(new Blob([script], { type: 'text/x-shellscript' }));
		const a = document.createElement('a'); a.href = url; a.download = `${plan?.id ?? 'plan'}-${hostId}.sh`; a.click();
		URL.revokeObjectURL(url);
	}
</script>

<PageHeader title={t('provisionTitle')} intro={t('provisionIntro')} />
<div class="space-y-6">
	<Card.Root><Card.Header><Card.Title>{t('provisionHosts')}</Card.Title><Card.Description>{t('provisionSafety')}</Card.Description></Card.Header>
		<Card.Content><form onsubmit={preflight} class="space-y-5">
			<div class="grid gap-4 sm:grid-cols-2">
				<div class="space-y-1"><Label for="plan-id">{t('provisionId')}</Label><Input id="plan-id" bind:value={form.id} required /></div>
				<div class="space-y-1"><Label for="platform">{t('platform')}</Label><select id="platform" class={selectClass} bind:value={form.platform}><option value="swarm">Docker Swarm</option><option value="kubernetes">Kubernetes (k3s preview)</option></select></div>
			</div>
			{#each form.hosts as host, i (host.id)}
				<fieldset class="space-y-3 rounded border p-4" disabled={busy}>
					<legend class="px-1 font-medium">{t('provisionHost')} {i + 1}</legend>
					<div class="grid gap-3 sm:grid-cols-3">
						<div class="space-y-1"><Label for={`addr-${i}`}>{t('provisionAddress')}</Label><Input id={`addr-${i}`} bind:value={host.address} required /></div>
						<div class="space-y-1"><Label for={`user-${i}`}>{t('provisionUser')}</Label><Input id={`user-${i}`} bind:value={host.user} required /></div>
						<div class="space-y-1"><Label for={`role-${i}`}>{t('provisionRole')}</Label><select id={`role-${i}`} class={selectClass} bind:value={host.role} disabled={i === 0}><option value="manager">{t('provisionManager')}</option><option value="worker">{t('provisionWorker')}</option></select></div>
					</div>
					<div class="space-y-1"><Label for={`key-${i}`}>{t('provisionPrivateKey')}</Label><textarea id={`key-${i}`} class="min-h-20 w-full rounded border bg-background p-2 font-mono text-xs" bind:value={form.credentials[host.id].privateKey} required autocomplete="off" spellcheck={false}></textarea></div>
					<div class="space-y-1"><Label for={`known-${i}`}>{t('provisionKnownHosts')}</Label><textarea id={`known-${i}`} class="min-h-20 w-full rounded border bg-background p-2 font-mono text-xs" bind:value={form.credentials[host.id].knownHosts} required autocomplete="off" spellcheck={false}></textarea></div>
					{#if i > 0}<Button type="button" variant="outline" onclick={() => removeHost(i)}>{t('provisionRemove')}</Button>{/if}
				</fieldset>
			{/each}
			<div class="flex gap-3"><Button type="button" variant="outline" onclick={addHost}>{t('provisionAddHost')}</Button><Button type="submit" disabled={busy}>{t('provisionCheck')}</Button></div>
			{#if touched && !validProvision(form) && !plan}<p role="alert" class="text-sm text-destructive">{t('provisionInvalid')}</p>{/if}
		</form></Card.Content>
	</Card.Root>
	{#if error}<ApiGate {error} />{/if}
	{#if plan}
		<Card.Root><Card.Header><Card.Title>{t('provisionReview')}</Card.Title><Card.Description>{t('provisionDigest')}: <code class="break-all">{plan.digest}</code></Card.Description></Card.Header>
			<Card.Content class="space-y-4">
				{#each plan.checks as c}<p class="text-sm">{c.hostId}: {c.os}, {t('provisionDisk')} {Math.floor(c.diskBytes / 1024 ** 3)} GiB, {t('provisionPorts')} {c.portsAvailable ? t('yes') : t('no')}, {t('provisionTime')} {c.timeSynchronized ? t('yes') : t('no')}</p>{/each}
				{#each plan.steps as s}<div class="rounded border p-3"><p class="font-medium">{s.hostId} · {s.action}</p><pre class="overflow-x-auto text-xs">{s.command}</pre><Button type="button" variant="outline" onclick={() => download(s.hostId, plan!.scripts[s.hostId])}>{t('provisionDownload')}</Button></div>{/each}
				{#if plan.platform === 'swarm'}<p class="text-sm">{t('provisionApproval')}</p><Button type="button" onclick={execute} disabled={busy || !!started}>{t('provisionQueue')}</Button>
				{:else}<p class="text-sm">{t('provisionKubernetesLimit')}</p>{/if}
				{#if started}<p role="status">{t('provisionQueued')} <a class="underline" href="/operations">{started}</a></p>{/if}
			</Card.Content>
		</Card.Root>
	{/if}
</div>
