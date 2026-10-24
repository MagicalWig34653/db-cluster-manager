<script lang="ts">
 import { api, asApiError, type ApiError } from '../../../lib/api';
 import { emptyRegistration, registrationInput, validRegistration } from '../../../lib/kubernetes';
 import { load } from '../../../lib/load.svelte';
 import { t } from '../../../lib/locale.svelte';
 import PageHeader from '../../../lib/components/app/PageHeader.svelte';
 import ApiGate from '../../../lib/components/app/ApiGate.svelte';
 import Loading from '../../../lib/components/app/Loading.svelte';
 import * as Card from '../../../lib/components/ui/card/index.js';
 import * as Alert from '../../../lib/components/ui/alert/index.js';
 import * as Table from '../../../lib/components/ui/table/index.js';
 import { Button } from '../../../lib/components/ui/button/index.js';
 import { Input } from '../../../lib/components/ui/input/index.js';
 import { Label } from '../../../lib/components/ui/label/index.js';
 import { selectClass } from '../../../lib/components/app/fieldClass';
 const registrations = load(api.kubernetesRegistrations);
 let form = $state(emptyRegistration());
 let busy = $state(false);
 let touched = $state(false);
 let saved = $state(false);
 let error = $state<ApiError | undefined>();
 async function register(e: SubmitEvent) {
  e.preventDefault(); touched = true; saved = false;
  if (!validRegistration(form) || busy) return;
  busy = true; error = undefined;
  try {
   // Request credentials stay in memory only and are cleared even on failure.
   await api.registerKubernetes(registrationInput(form));
   form = emptyRegistration(); touched = false; saved = true; registrations.refresh();
  } catch (e) { error = asApiError(e); }
  finally { form.credential = ''; busy = false; }
 }
</script>

<PageHeader title={t('kubeTitle')} intro={t('kubeIntro')} />
<div class="space-y-6">
 {#if registrations.state.status === 'loading'}<Loading />
 {:else if registrations.state.status === 'error'}<ApiGate error={registrations.state.error} retry={registrations.reload} />
 {:else}
  {#if registrations.state.value.length === 0}<p>{t('kubeEmpty')}</p>
  {:else}
   <div class="rounded-lg border"><Table.Root>
    <Table.Header><Table.Row>
     <Table.Head>ID</Table.Head><Table.Head>{t('colName')}</Table.Head><Table.Head>{t('kubeEndpoint')}</Table.Head><Table.Head>{t('kubeNamespace')}</Table.Head><Table.Head>{t('kubeKind')}</Table.Head><Table.Head>{t('kubeStored')}</Table.Head>
    </Table.Row></Table.Header>
    <Table.Body>{#each registrations.state.value as r (r.id)}<Table.Row>
     <Table.Cell>{r.id}</Table.Cell><Table.Cell>{r.name}</Table.Cell><Table.Cell class="break-all font-mono text-xs">{r.endpoint}</Table.Cell><Table.Cell>{r.namespace}</Table.Cell><Table.Cell>{t(r.credentialKind === 'token' ? 'kubeToken' : 'kubeConfig')}</Table.Cell><Table.Cell>{t(r.credentialSet ? 'yes' : 'no')}</Table.Cell>
    </Table.Row>{/each}</Table.Body>
   </Table.Root></div>
  {/if}
  <Card.Root><Card.Header><Card.Title><h2>{t('kubeRegister')}</h2></Card.Title><Card.Description>{t('kubeSecretHint')}</Card.Description></Card.Header>
   <Card.Content><form onsubmit={register} class="max-w-2xl space-y-4">
    <fieldset disabled={busy} class="space-y-4">
     <div class="space-y-1.5"><Label for="k-id">ID</Label><Input id="k-id" bind:value={form.id} required maxlength={63} /></div>
     <div class="space-y-1.5"><Label for="k-name">{t('colName')}</Label><Input id="k-name" bind:value={form.name} required maxlength={100} /></div>
     <div class="space-y-1.5"><Label for="k-endpoint">{t('kubeEndpoint')}</Label><Input id="k-endpoint" bind:value={form.endpoint} required placeholder="https://kube.example.invalid:6443" /></div>
     <div class="space-y-1.5"><Label for="k-namespace">{t('kubeNamespace')}</Label><Input id="k-namespace" bind:value={form.namespace} required maxlength={63} /></div>
     <div class="space-y-1.5"><Label for="k-kind">{t('kubeKind')}</Label><select id="k-kind" class={selectClass} bind:value={form.kind} onchange={() => { form.credential = ''; }}><option value="token">{t('kubeToken')}</option><option value="kubeconfig">{t('kubeConfig')}</option></select></div>
     <div class="space-y-1.5"><Label for="k-secret">{t('kubeCredential')}</Label>
      {#if form.kind === 'token'}<Input id="k-secret" type="password" bind:value={form.credential} required autocomplete="off" />
      {:else}<textarea id="k-secret" class="min-h-32 w-full rounded border bg-background p-2 font-mono text-xs" bind:value={form.credential} required autocomplete="off" spellcheck={false}></textarea>{/if}
     </div>
     {#if touched && !validRegistration(form)}<p role="alert" class="text-sm text-destructive">{t('kubeInvalid')}</p>{/if}
     <Button type="submit" disabled={busy}>{t('kubeRegister')}</Button>
    </fieldset>
    <div aria-live="polite">{#if error}<ApiGate {error} />{/if}{#if saved}<Alert.Root><Alert.Description>{t('kubeSaved')}</Alert.Description></Alert.Root>{/if}</div>
   </form></Card.Content>
  </Card.Root>
 {/if}
</div>
