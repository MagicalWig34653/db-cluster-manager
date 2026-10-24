<script lang="ts">
 import { api } from '../../../../lib/api';
 import { auditRecords } from '../../../../lib/vuln';
 import { load } from '../../../../lib/load.svelte';
 import { locale, t } from '../../../../lib/locale.svelte';
 import { dateTime } from '../../../../lib/format';
 import PageHeader from '../../../../lib/components/app/PageHeader.svelte';
 import ApiGate from '../../../../lib/components/app/ApiGate.svelte';
 import Loading from '../../../../lib/components/app/Loading.svelte';
 import { Button } from '../../../../lib/components/ui/button/index.js';
 import { Input } from '../../../../lib/components/ui/input/index.js';
 import { Label } from '../../../../lib/components/ui/label/index.js';
 import * as Table from '../../../../lib/components/ui/table/index.js';
 const audit = load(api.vulnAudit);
 let query = $state('');
 const rows = $derived(audit.state.status === 'ok' ? auditRecords(audit.state.value, query) : []);
</script>
<PageHeader title={t('vulnAudit')} intro={t('vulnAuditIntro')}>
 {#snippet actions()}<Button href="/vulnerabilities" variant="outline">{t('vulnTitle')}</Button><Button variant="outline" onclick={audit.reload}>{t('refresh')}</Button>{/snippet}
</PageHeader>
<div class="space-y-4">
 <p class="text-sm text-muted-foreground">{t('vulnAuditHint')}</p>
 {#if audit.state.status === 'loading'}<Loading />
 {:else if audit.state.status === 'error'}<ApiGate error={audit.state.error} retry={audit.reload} />
 {:else}
  <div class="max-w-lg space-y-1.5"><Label for="audit-search">{t('vulnAuditSearch')}</Label><Input id="audit-search" type="search" bind:value={query} /></div>
  {#if rows.length === 0}<p>{t('vulnAuditNoMatches')}</p>
  {:else}<div class="rounded-lg border"><Table.Root>
   <Table.Header><Table.Row><Table.Head>{t('colTime')}</Table.Head><Table.Head>{t('auditActor')}</Table.Head><Table.Head>{t('auditEvent')}</Table.Head><Table.Head>{t('auditResult')}</Table.Head><Table.Head>{t('vulnAuditReferences')}</Table.Head></Table.Row></Table.Header>
   <Table.Body>{#each rows as r (r.id)}<Table.Row>
    <Table.Cell class="whitespace-nowrap">{dateTime(r.at, locale.value)}</Table.Cell><Table.Cell>{r.actor}</Table.Cell>
    <Table.Cell><span class="font-mono text-xs">{r.event}</span>{#if r.detail}<p class="max-w-lg break-words text-xs text-muted-foreground">{r.detail}</p>{/if}</Table.Cell><Table.Cell>{r.result}</Table.Cell>
    <Table.Cell class="break-all text-xs">{#if r.findingId}<a class="underline" href={'/vulnerabilities/' + encodeURIComponent(r.findingId)}>{r.findingId}</a>{/if}
     {#each [r.proposalId, r.provider, r.model, r.planDigest].filter(Boolean) as reference}<p class="font-mono">{reference}</p>{/each}
    </Table.Cell>
   </Table.Row>{/each}</Table.Body>
  </Table.Root></div>{/if}
 {/if}
</div>
