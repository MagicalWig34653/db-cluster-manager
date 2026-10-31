<script lang="ts">
	import ArrowLeft from '@lucide/svelte/icons/arrow-left';
	import { api, asApiError, type ApiError } from '../../../../lib/api';
	import { keyringSummary, replicaState, replicasOrdered, validReplicaKey, type ReplicaState } from '../../../../lib/platform';
	import { load } from '../../../../lib/load.svelte';
	import { t } from '../../../../lib/locale.svelte';
	import type { MessageKey } from '../../../../lib/i18n';
	import * as Card from '../../../../lib/components/ui/card/index.js';
	import * as Table from '../../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../../lib/components/ui/button/index.js';
	import { Input } from '../../../../lib/components/ui/input/index.js';
	import { Label } from '../../../../lib/components/ui/label/index.js';
	import { selectClass } from '../../../../lib/components/app/fieldClass';
	import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
	import Freshness from '../../../../lib/components/app/Freshness.svelte';
	import ApiGate from '../../../../lib/components/app/ApiGate.svelte';
	import ErrorNotice from '../../../../lib/components/app/ErrorNotice.svelte';
	import Loading from '../../../../lib/components/app/Loading.svelte';
	import KeyRotationPanel from '../../../../lib/components/app/KeyRotationPanel.svelte';
	import MembershipPanel from '../../../../lib/components/app/MembershipPanel.svelte';

	const keyring = load(api.keyring);
	const stateLabel: Record<ReplicaState, MessageKey> = { ready: 'keyStateReady', noKey: 'keyStateNoKey', unregistered: 'keyStateUnregistered' };
	const stateClass: Record<ReplicaState, string> = {
		ready: 'text-emerald-700 dark:text-emerald-400',
		noKey: 'text-amber-700 dark:text-amber-400',
		unregistered: 'text-muted-foreground'
	};

	let replica = $state('');
	let publicKey = $state('');
	let touched = $state(false);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let done = $state<string | undefined>();
	const keyOk = $derived(validReplicaKey(publicKey));
	async function register(e: SubmitEvent) {
		e.preventDefault();
		touched = true;
		done = undefined;
		if (!keyOk || replica === '') return;
		busy = true;
		error = undefined;
		try {
			const r = await api.registerReplicaKey(replica, publicKey.trim());
			done = t('keyRegistered', { id: r.id });
			publicKey = '';
			touched = false;
			keyring.refresh();
		} catch (x) {
			error = asApiError(x);
		} finally {
			busy = false;
		}
	}

	let revoking = $state<string | undefined>();
	let revokeBusy = $state(false);
	let revokeError = $state<ApiError | undefined>();
	let revoked = $state<string | undefined>();
	async function revoke(id: string) {
		revokeBusy = true;
		revokeError = undefined;
		revoked = undefined;
		try {
			const r = await api.revokeReplicaCertificates(id);
			revoked = t('keyRevoked', { count: r.revoked, id: r.replica });
		} catch (x) {
			revokeError = asApiError(x);
		} finally {
			revokeBusy = false;
			revoking = undefined;
		}
	}
</script>

<div class="mb-3">
	<a href="/settings" class="inline-flex items-center gap-1 text-sm text-muted-foreground underline-offset-4 hover:underline"><ArrowLeft class="size-4" aria-hidden="true" />{t('navSettings')}</a>
</div>
<PageHeader title={t('keyTitle')} intro={t('keyIntro')} />

<div class="space-y-6">
	{#if keyring.state.status === 'loading'}
		<Loading />
	{:else if keyring.state.status === 'error'}
		<ApiGate error={keyring.state.error} retry={keyring.reload} />
	{:else}
		{@const k = keyring.state.value}
		{@const sum = keyringSummary(k)}
		<Card.Root>
			<Card.Content class="space-y-3">
				<dl class="grid grid-cols-[minmax(10rem,auto)_1fr] gap-x-4 gap-y-2 text-sm">
					<dt class="text-muted-foreground">{t('keyCurrent')}</dt><dd class="font-mono text-xs">{k.currentKey}</dd>
					<dt class="text-muted-foreground">{t('keyRecoverable')}</dt><dd>{k.recoverable ? t('keyRecoverableYes') : t('keyRecoverableNo')}</dd>
				</dl>
				<p class="text-sm font-medium">{t('keySummary', { ready: sum.ready, total: sum.replicas })}</p>
				{#if !k.recoverable}<Alert.Root variant="destructive"><Alert.Description>{t('keyRecoverableNo')}</Alert.Description></Alert.Root>{/if}
			</Card.Content>
		</Card.Root>

		{#if k.replicas.length === 0}
			<p class="text-sm text-muted-foreground">{t('keyEmpty')}</p>
		{:else}
			<div class="rounded-lg border">
				<Table.Root>
					<Table.Header>
						<Table.Row>
							<Table.Head>{t('keyColReplica')}</Table.Head>
							<Table.Head>{t('keyColAddress')}</Table.Head>
							<Table.Head>{t('keyColIdentity')}</Table.Head>
							<Table.Head>{t('keyColRegistered')}</Table.Head>
							<Table.Head>{t('keyColKey')}</Table.Head>
							<Table.Head>{t('colActions')}</Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each replicasOrdered(k.replicas) as r (r.id)}
							{@const st = replicaState(r)}
							<Table.Row>
								<Table.Cell class="font-medium">{r.id}{#if r.leader}<span class="ml-1 rounded bg-primary/10 px-1.5 py-0.5 text-xs">{t('keyLeader')}</span>{/if}</Table.Cell>
								<Table.Cell class="font-mono text-xs">{r.address ?? t('notReported')}</Table.Cell>
								<Table.Cell class="font-mono text-xs">{r.publicKeyId ?? '—'}</Table.Cell>
								<Table.Cell>{#if r.registeredAt}<Freshness plain at={r.registeredAt} />{#if r.registeredBy}<span class="text-xs text-muted-foreground"> · {r.registeredBy}</span>{/if}{:else}—{/if}</Table.Cell>
								<Table.Cell><span class="font-medium {stateClass[st]}">{t(stateLabel[st])}</span></Table.Cell>
								<Table.Cell>
									{#if revoking !== r.id}
										<Button size="sm" variant="outline" onclick={() => (revoking = r.id)}>{t('keyRevoke')}</Button>
									{:else}
										<Alert.Root variant="destructive">
											<Alert.Description class="space-y-2">
												<p>{t('keyRevokeConfirm', { id: r.id })}</p>
												<div class="flex gap-2">
													<Button size="sm" variant="destructive" disabled={revokeBusy} onclick={() => revoke(r.id)}>{t('keyRevokeYes')}</Button>
													<Button size="sm" variant="ghost" onclick={() => (revoking = undefined)}>{t('cancel')}</Button>
												</div>
											</Alert.Description>
										</Alert.Root>
									{/if}
								</Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			</div>
			<p class="text-xs text-muted-foreground">{t('keyRevokeIntro')}</p>
			<div aria-live="polite" class="space-y-2">
				{#if revokeError}<ErrorNotice error={revokeError} />{/if}
				{#if revoked}<Alert.Root><Alert.Description>{revoked}</Alert.Description></Alert.Root>{/if}
			</div>
		{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title><h2>{t('keyRegister')}</h2></Card.Title>
				<Card.Description>{t('keyRegisterIntro')}</Card.Description>
			</Card.Header>
			<Card.Content>
				<form class="max-w-2xl space-y-3" onsubmit={register} aria-label={t('keyRegister')}>
					<div class="space-y-1.5">
						<Label for="rk-replica">{t('keyReplicaId')}</Label>
						<select id="rk-replica" class={selectClass} bind:value={replica}>
							<option value="">{t('choose')}</option>
							{#each replicasOrdered(k.replicas) as r (r.id)}<option value={r.id}>{r.id}</option>{/each}
						</select>
					</div>
					<div class="space-y-1.5">
						<Label for="rk-key">{t('keyPublicKey')}</Label>
						<Input id="rk-key" class="font-mono" bind:value={publicKey} autocomplete="off" spellcheck={false} aria-invalid={touched && !keyOk} placeholder={t('examplePublicKey')} />
						{#if touched && !keyOk}<p class="text-xs text-red-700 dark:text-red-400">{t('valKeyFormat')}</p>{/if}
					</div>
					<Button type="submit" disabled={busy}>{t('keyRegisterBtn')}</Button>
					<div aria-live="polite" class="space-y-2">
						{#if error}<ApiGate {error} />{/if}
						{#if done}<Alert.Root><Alert.Description>{done}</Alert.Description></Alert.Root>{/if}
					</div>
				</form>
			</Card.Content>
		</Card.Root>

		<KeyRotationPanel onstarted={keyring.refresh} />
		<MembershipPanel replicas={k.replicas.map((r) => r.id)} onchanged={keyring.refresh} />
	{/if}
</div>
