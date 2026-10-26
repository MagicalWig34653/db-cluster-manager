<script lang="ts">
	import { api, asApiError, type ApiError, type KeyRotationResult } from '../../api';
	import { approvableDigest } from '../../clusterplan';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import * as Card from '../ui/card/index.js';
	import { Button } from '../ui/button/index.js';
	import ApiGate from './ApiGate.svelte';
	import { checkClass } from './fieldClass';

	// Two steps: preview returns a revision-bound digest, approval resubmits it.
	let { onstarted }: { onstarted?: () => void } = $props();

	let preview = $state<KeyRotationResult | undefined>();
	let started = $state<KeyRotationResult | undefined>();
	let confirmed = $state(false);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();

	async function show() {
		busy = true;
		error = undefined;
		started = undefined;
		confirmed = false;
		try {
			preview = await api.rotateDataKey();
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
			started = await api.rotateDataKey(preview.digest);
			preview = undefined;
			confirmed = false;
			onstarted?.();
		} catch (x) {
			error = asApiError(x);
			// A stale digest or active rotation needs a fresh preview.
			if (error.status === 409) preview = undefined;
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title><h2>{t('rotTitle')}</h2></Card.Title>
		<Card.Description>{t('rotIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="max-w-2xl space-y-3 text-sm">
		<Button variant="outline" disabled={busy} onclick={show}>{t('rotPreview')}</Button>
		<div aria-live="polite" class="space-y-3">
			{#if preview}
				<p class="text-xs text-muted-foreground">{t('keyCurrent')}: <code class="font-mono">{preview.currentKey}</code></p>
				<p class="text-xs text-muted-foreground">{t('rotDigest')}: <code class="font-mono break-all">{preview.digest}</code></p>
				<label class="flex items-start gap-2"><input type="checkbox" class="{checkClass} mt-0.5" bind:checked={confirmed} /><span>{t('rotConfirm', { digest: preview.digest.slice(0, 12) })}</span></label>
				<Button variant="destructive" disabled={!approvableDigest(preview.digest, confirmed, busy)} onclick={approve}>{t('rotApprove')}</Button>
			{/if}
			{#if started}<Alert.Root><Alert.Description>{t('rotStarted', { key: started.currentKey })}</Alert.Description></Alert.Root>{/if}
			{#if error}<ApiGate {error} />{/if}
		</div>
	</Card.Content>
</Card.Root>
