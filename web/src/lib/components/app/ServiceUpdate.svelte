<script lang="ts">
 import { api, asApiError, type ApiError, type ServiceInfo, type ServicePlan, type ServiceStarted, type ServiceUpdateInput } from '../../api';
 import { defaultStrategy, imagePinned, planView } from '../../services';
 import { t } from '../../locale.svelte';
 import type { MessageKey } from '../../i18n';
 import { Button } from '../ui/button/index.js';
 import { selectClass, checkClass } from './fieldClass';
 import PlanBlock from './PlanBlock.svelte';
 import ApiGate from './ApiGate.svelte';
 import FollowOperation from './FollowOperation.svelte';
 let { service, refresh }: { service: ServiceInfo; refresh: () => void } = $props();
 const initialImage = () => typeof service.spec.image === 'string' ? service.spec.image : '';
 let image = $state(initialImage());
 let rollback = $state(false);
 let strategy = $state(defaultStrategy());
 let canary = $state(false);
 let canaryReplicas = $state(1);
 let holdSeconds = $state(30);
 let shown = $state<{ plan: ServicePlan; request: string }>();
 let started = $state<ServiceStarted>();
 let error = $state<ApiError>();
 let busy = $state(false);
 const input = $derived<ServiceUpdateInput>({ ...(rollback ? { rollback: true } : { spec: { ...service.spec, image: image.trim() } }), strategy: { ...strategy, ...(canary ? { canary: { replicas: canaryReplicas, holdSeconds } } : {}) } });
 const request = $derived(JSON.stringify(input));
 const stale = $derived(shown?.request !== request);
 const numeric: { key: 'parallelism' | 'delaySeconds' | 'monitorSeconds' | 'maxFailureRatio'; label: MessageKey; min: number; max: number; step: number }[] = [
  { key: 'parallelism', label: 'svcParallelism', min: 1, max: 50, step: 1 },
  { key: 'delaySeconds', label: 'svcDelay', min: 0, max: 3600, step: 1 },
  { key: 'monitorSeconds', label: 'svcMonitor', min: 1, max: 3600, step: 1 },
  { key: 'maxFailureRatio', label: 'svcFailureRatio', min: 0, max: 1, step: 0.01 }
 ];
 async function preview(e: SubmitEvent) {
  e.preventDefault(); if (!rollback && !imagePinned(image)) return;
  busy = true; error = undefined; started = undefined;
  const snapshot = request;
  try { shown = { plan: await api.previewServiceUpdate(service.id, JSON.parse(snapshot)), request: snapshot }; }
  catch (e) { shown = undefined; error = asApiError(e); } finally { busy = false; }
 }
 async function start() {
  if (!shown || stale || !planView(shown.plan).canStart) return;
  busy = true; error = undefined;
  try { started = await api.updateService(service.id, JSON.parse(shown.request)); refresh(); }
  catch (e) { error = asApiError(e); } finally { busy = false; }
 }
</script>
<section class="space-y-4 rounded-lg border p-4" aria-label={t('svcUpdate')}>
 <h2 class="font-semibold">{t('svcUpdate')}: {service.name}</h2>
 <p class="text-sm text-muted-foreground">{t('svcUpdateHint')}</p>
 <form onsubmit={preview} class="space-y-3">
  <label class="flex gap-2 text-sm"><input type="checkbox" class={checkClass} disabled={!service.rollbackable} bind:checked={rollback} />{t('svcRollback')}</label>
  {#if !rollback}<label class="block text-sm">{t('svcImage')}<input class={selectClass} required bind:value={image} aria-invalid={!imagePinned(image)} /></label>{/if}
  <div class="grid gap-3 md:grid-cols-3">
   <label class="text-sm">{t('svcOrder')}<select class={selectClass} bind:value={strategy.order}><option value="stop-first">{t('stopFirst')}</option><option value="start-first">{t('startFirst')}</option></select></label>
   <label class="text-sm">{t('svcOnFailure')}<select class={selectClass} bind:value={strategy.onFailure}><option value="rollback">{t('svcRollback')}</option><option value="pause">{t('svcPause')}</option></select></label>
   {#each numeric as f (f.key)}<label class="text-sm">{t(f.label)}<input class={selectClass} type="number" required min={f.min} max={f.max} step={f.step} bind:value={strategy[f.key]} /></label>{/each}
  </div>
  <label class="flex gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={canary} />{t('svcCanary')}</label>
  {#if canary}<div class="grid gap-3 md:grid-cols-2">
   <label class="text-sm">{t('svcCanaryReplicas')}<input class={selectClass} required type="number" min="1" step="1" bind:value={canaryReplicas} /></label>
   <label class="text-sm">{t('svcCanaryHold')}<input class={selectClass} required type="number" min="0" max="3600" step="1" bind:value={holdSeconds} /></label>
  </div>{/if}
  <Button type="submit" disabled={busy || (!rollback && !imagePinned(image))}>{t('svcPreview')}</Button>
 </form>
 {#if error}<ApiGate {error} />{/if}
 {#if shown}
  <PlanBlock plan={shown.plan} />
  {#if stale}<p role="status">{t('svcPlanStale')}</p>{:else if !started}<Button disabled={busy || !planView(shown.plan).canStart} onclick={start}>{t('svcRequestUpdate')}</Button>{/if}
 {/if}
 {#if started}<PlanBlock plan={started.plan} /><FollowOperation id={started.operation.id} />{/if}
</section>
