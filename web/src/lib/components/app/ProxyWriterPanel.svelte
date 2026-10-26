<script lang="ts">
	import { api, asApiError, type ApiError, type ClusterNode, type ProxyWriterChoice } from '../../api';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import * as Card from '../ui/card/index.js';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import ApiGate from './ApiGate.svelte';
	import { checkClass, selectClass } from './fieldClass';

	// Coordinated writer switch behind the proxies. It needs the separate writer
	// administrator token, which stays in memory only and is cleared after use.
	let { cluster, nodes }: { cluster: string; nodes: ClusterNode[] } = $props();

	let writer = $state('');
	let token = $state('');
	let confirmed = $state(false);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let choice = $state<ProxyWriterChoice | undefined>();

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (!writer || token === '' || !confirmed) return;
		busy = true;
		error = undefined;
		try {
			choice = await api.switchProxyWriter(cluster, writer, token);
			confirmed = false;
		} catch (x) {
			error = asApiError(x);
		} finally {
			token = '';
			busy = false;
		}
	}
</script>

<Card.Root class="lg:col-span-2">
	<Card.Header>
		<Card.Title><h2>{t('pwTitle')}</h2></Card.Title>
		<Card.Description>{t('pwIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="space-y-3 text-sm">
		<form class="max-w-2xl space-y-3" onsubmit={submit} aria-label={t('pwTitle')}>
			<div class="space-y-1.5">
				<Label for="pw-writer">{t('pwWriter')}</Label>
				<select id="pw-writer" class={selectClass} bind:value={writer}>
					<option value="">{t('choose')}</option>
					{#each nodes as n (n.id)}<option value={n.id}>{n.id}</option>{/each}
				</select>
			</div>
			<div class="space-y-1.5">
				<Label for="pw-token">{t('pwToken')}</Label>
				<Input id="pw-token" type="password" bind:value={token} autocomplete="off" spellcheck={false} />
			</div>
			<label class="flex items-start gap-2"><input type="checkbox" class="{checkClass} mt-0.5" bind:checked={confirmed} /><span>{t('pwConfirm')}</span></label>
			<Button type="submit" variant="destructive" disabled={busy || !writer || token === '' || !confirmed}>{t('pwSwitch')}</Button>
		</form>
		<div aria-live="polite" class="space-y-2">
			{#if error}<ApiGate {error} />{/if}
			{#if choice}
				<Alert.Root>
					<Alert.Description class="space-y-1">
						<p>{choice.pending ? t('pwResult', { epoch: choice.epoch, pending: choice.pending }) : t('pwUnchanged', { epoch: choice.epoch })}</p>
						{#if choice.acks && Object.keys(choice.acks).length > 0}
							<p class="text-xs text-muted-foreground">{t('pwAcks')}: {Object.entries(choice.acks).map(([p, ok]) => `${p} ${ok ? '✓' : '…'}`).join(', ')}</p>
						{/if}
					</Alert.Description>
				</Alert.Root>
			{/if}
		</div>
	</Card.Content>
</Card.Root>
