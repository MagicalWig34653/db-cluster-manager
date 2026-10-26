<script lang="ts">
	import { api, asApiError, type ApiError, type MembershipResult } from '../../api';
	import { approvableDigest } from '../../clusterplan';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import * as Card from '../ui/card/index.js';
	import { Button } from '../ui/button/index.js';
	import { Label } from '../ui/label/index.js';
	import ApiGate from './ApiGate.svelte';
	import { checkClass, selectClass } from './fieldClass';

	// Preview one Raft member change, then approve exactly the previewed digest.
	let { replicas, onchanged }: { replicas: string[]; onchanged?: () => void } = $props();

	let replica = $state('');
	let remove = $state(false);
	let preview = $state<MembershipResult | undefined>();
	let confirmed = $state(false);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let done = $state<string | undefined>();

	async function show(rm: boolean) {
		if (replica === '') return;
		busy = true;
		error = undefined;
		done = undefined;
		confirmed = false;
		remove = rm;
		try {
			preview = await api.changeMembership(replica, rm);
		} catch (x) {
			preview = undefined;
			error = asApiError(x);
		} finally {
			busy = false;
		}
	}
	async function approve() {
		if (!preview || !approvableDigest(preview.digest, confirmed, busy)) return;
		busy = true;
		error = undefined;
		try {
			const r = await api.changeMembership(replica, remove, preview.digest);
			if (r.complete) done = t('memComplete', { id: replica });
			preview = undefined;
			confirmed = false;
			onchanged?.();
		} catch (x) {
			error = asApiError(x);
			if (error.status === 409) preview = undefined;
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title><h2>{t('memTitle')}</h2></Card.Title>
		<Card.Description>{t('memIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="max-w-2xl space-y-3 text-sm">
		<div class="space-y-1.5">
			<Label for="mem-replica">{t('keyReplicaId')}</Label>
			<select id="mem-replica" class={selectClass} bind:value={replica} onchange={() => { preview = undefined; done = undefined; }}>
				<option value="">{t('choose')}</option>
				{#each replicas as r (r)}<option value={r}>{r}</option>{/each}
			</select>
		</div>
		<div class="flex flex-wrap gap-2">
			<Button variant="outline" disabled={busy || replica === ''} onclick={() => show(false)}>{t('memAdd')}</Button>
			<Button variant="outline" disabled={busy || replica === ''} onclick={() => show(true)}>{t('memRemove')}</Button>
		</div>
		<p class="text-xs text-muted-foreground">{t('memNotice')}</p>
		<div aria-live="polite" class="space-y-3">
			{#if preview}
				<p class="text-xs text-muted-foreground">{t('rotDigest')}: <code class="font-mono break-all">{preview.digest}</code></p>
				<label class="flex items-start gap-2">
					<input type="checkbox" class="{checkClass} mt-0.5" bind:checked={confirmed} />
					<span>{remove ? t('memConfirmRemove', { id: replica, digest: preview.digest.slice(0, 12) }) : t('memConfirmAdd', { id: replica, digest: preview.digest.slice(0, 12) })}</span>
				</label>
				<Button variant={remove ? 'destructive' : 'default'} disabled={!approvableDigest(preview.digest, confirmed, busy)} onclick={approve}>{t('memApprove')}</Button>
			{/if}
			{#if done}<Alert.Root><Alert.Description>{done}</Alert.Description></Alert.Root>{/if}
			{#if error}<ApiGate {error} />{/if}
		</div>
	</Card.Content>
</Card.Root>
