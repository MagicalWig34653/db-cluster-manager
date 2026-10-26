<script lang="ts">
	import { api, asApiError, clusterOperationKinds, type ApiError, type ClusterNode, type ClusterOperationKind, type ClusterOperationPlan } from '../../api';
	import { clusterOperationInput, clusterOperationLabel, needsImage, needsSlot, planInputProblem, scaleInBelowThree } from '../../clusterplan';
	import type { MessageKey } from '../../i18n';
	import { t } from '../../locale.svelte';
	import * as Alert from '../ui/alert/index.js';
	import * as Card from '../ui/card/index.js';
	import { Button } from '../ui/button/index.js';
	import { Input } from '../ui/input/index.js';
	import { Label } from '../ui/label/index.js';
	import ApiGate from './ApiGate.svelte';
	import FollowOperation from './FollowOperation.svelte';
	import { selectClass } from './fieldClass';

	// Plans a Galera scale-out/in, rolling restart or patch upgrade. The plan is
	// only stored; the approval (with the plan digest) happens on the operation card.
	let { cluster, nodes, onplanned }: { cluster: string; nodes: ClusterNode[]; onplanned?: () => void } = $props();

	let kind = $state<ClusterOperationKind>('rolling-restart');
	let slot = $state('');
	let image = $state('');
	let touched = $state(false);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let planned = $state<ClusterOperationPlan | undefined>();

	const slots = $derived(nodes.map((n) => n.id));
	const problem = $derived(planInputProblem(kind, slot, image, slots));
	const problemKey: Record<string, MessageKey> = { slot: 'cpErrSlot', slotExists: 'cpErrSlotExists', slotMissing: 'cpErrSlotMissing', image: 'cpErrImage' };

	async function plan(e: SubmitEvent) {
		e.preventDefault();
		touched = true;
		if (problem) return;
		busy = true;
		error = undefined;
		planned = undefined;
		try {
			planned = await api.planClusterOperation(cluster, clusterOperationInput(kind, slot, image));
			onplanned?.();
		} catch (x) {
			error = asApiError(x);
		} finally {
			busy = false;
		}
	}
</script>

<Card.Root class="lg:col-span-2">
	<Card.Header>
		<Card.Title><h2>{t('cpTitle')}</h2></Card.Title>
		<Card.Description>{t('cpIntro')}</Card.Description>
	</Card.Header>
	<Card.Content class="space-y-4 text-sm">
		<form class="max-w-2xl space-y-3" onsubmit={plan} aria-label={t('cpTitle')}>
			<div class="space-y-1.5">
				<Label for="cp-kind">{t('cpKind')}</Label>
				<select id="cp-kind" class={selectClass} bind:value={kind} onchange={() => { touched = false; planned = undefined; }}>
					{#each clusterOperationKinds as k (k)}<option value={k}>{t(clusterOperationLabel[k])}</option>{/each}
				</select>
			</div>
			{#if kind === 'scale-out'}
				<div class="space-y-1.5">
					<Label for="cp-slot">{t('cpSlotNew')}</Label>
					<Input id="cp-slot" bind:value={slot} autocomplete="off" spellcheck={false} aria-invalid={touched && !!problem} placeholder="node-4" />
				</div>
			{:else if kind === 'scale-in'}
				<div class="space-y-1.5">
					<Label for="cp-slot-in">{t('cpSlotExisting')}</Label>
					<select id="cp-slot-in" class={selectClass} bind:value={slot} aria-invalid={touched && !!problem}>
						<option value="">{t('choose')}</option>
						{#each slots as s (s)}<option value={s}>{s}</option>{/each}
					</select>
				</div>
			{/if}
			{#if needsImage(kind)}
				<div class="space-y-1.5">
					<Label for="cp-image">{t('cpImage')}</Label>
					<Input id="cp-image" class="font-mono" bind:value={image} autocomplete="off" spellcheck={false} aria-invalid={touched && !!problem} />
					<p class="text-xs text-muted-foreground">{t('cpImageHint')}</p>
				</div>
			{/if}
			{#if touched && problem}<p class="text-xs text-red-700 dark:text-red-400">{t(problemKey[problem])}</p>{/if}
			{#if scaleInBelowThree(kind, nodes.length)}
				<Alert.Root variant="destructive"><Alert.Description>{t('cpBelowThree')}</Alert.Description></Alert.Root>
			{/if}
			<Button type="submit" disabled={busy}>{t('cpCreate')}</Button>
		</form>
		<div aria-live="polite" class="space-y-3">
			{#if error}<ApiGate {error} />{/if}
			{#if planned}
				<Alert.Root><Alert.Description>{t('cpPlanned', { id: planned.id })}</Alert.Description></Alert.Root>
				{#if planned.plan.steps?.length}
					<div>
						<h3 class="mb-1 font-medium">{t('cpSteps')}</h3>
						<ol class="list-decimal space-y-1 pl-5">
							{#each planned.plan.steps as s (s.id)}
								<li>
									<span class="font-mono text-xs">{s.kind}</span>{#if s.slot} <span class="text-muted-foreground">({s.slot})</span>{/if}
									{#if s.note}<span class="text-muted-foreground"> — {s.note}</span>{/if}
									{#if s.conditions?.length}<ul class="list-disc pl-5 text-xs text-muted-foreground">{#each s.conditions as c (c)}<li>{c}</li>{/each}</ul>{/if}
								</li>
							{/each}
						</ol>
					</div>
				{/if}
				{#if planned.plan.warnings?.length}
					<Alert.Root>
						<Alert.Title>{t('cpWarnings')}</Alert.Title>
						<Alert.Description><ul class="list-disc pl-5">{#each planned.plan.warnings as w (w)}<li>{w}</li>{/each}</ul></Alert.Description>
					</Alert.Root>
				{/if}
				<p class="text-muted-foreground">{t('cpApprovalHint')}</p>
				<FollowOperation id={planned.id} />
			{/if}
		</div>
	</Card.Content>
</Card.Root>
