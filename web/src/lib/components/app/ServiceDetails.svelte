<script lang="ts">
 import type { ServiceType } from '../../api';
 import { detailFields, type ServiceDetails } from '../../service-details';
 import { t } from '../../locale.svelte';
 import { selectClass, checkClass } from './fieldClass';
 import { Button } from '../ui/button/index.js';
 let { type, details = $bindable() }: { type: ServiceType; details: ServiceDetails } = $props();
 const inputClass = 'w-full rounded-md border bg-background px-3 py-2 text-sm';
</script>
<p class="text-sm text-muted-foreground">{t(type === 'galera' ? 'svcGaleraHint' : 'svcStorageHint')}</p>
<div class="grid gap-3 md:grid-cols-3">
 {#each detailFields(type) as f (f.key)}
  <label class="space-y-1 text-sm" for="detail-{f.key}">{t(f.label)}
   <input id="detail-{f.key}" class={inputClass} type="text" inputmode={f.number ? 'numeric' : 'text'} bind:value={details[f.key]} />
  </label>
 {/each}
</div>
{#if type === 'galera'}
 <label class="flex gap-2 text-sm"><input class={checkClass} type="checkbox" bind:checked={details.autoReplace} />{t('svcAutoReplace')}</label>
{:else}
 <label class="flex gap-2 text-sm"><input class={checkClass} type="checkbox" bind:checked={details.requireDigest} />{t('svcRequireDigest')}</label>
{/if}
{#if type === 'sftp' || type === 'ftp' || type === 's3'}
 <label class="block text-sm" for="detail-portMode">{t('svcPortMode')}</label>
 <select id="detail-portMode" class={selectClass} bind:value={details.portMode}><option value="host">host</option><option value="ingress">ingress</option></select>
{/if}
{#if type === 'ftp'}
 <label class="block text-sm" for="detail-mode">{t('svcFtpMode')}</label>
 <select id="detail-mode" class={selectClass} bind:value={details.mode}><option value="ftps-explicit">FTPS explicit</option><option value="ftps-implicit">FTPS implicit</option><option value="ftp">FTP</option></select>
 <label class="flex gap-2 text-sm"><input class={checkClass} type="checkbox" bind:checked={details.allowPlaintext} />{t('svcAllowPlaintext')}</label>
{/if}
{#if type === 's3'}
 <label class="flex gap-2 text-sm"><input class={checkClass} type="checkbox" bind:checked={details.console} />{t('svcConsole')}</label>
{/if}
{#if type === 'sftp' || type === 'ftp'}
 <fieldset class="space-y-3"><legend>{t('svcUsers')}</legend>
 {#each details.users as user, i (i)}
  <div class="grid gap-2 rounded border p-3 md:grid-cols-3">
   <label class="text-sm">{t('svcUserName')}<input class={inputClass} bind:value={user.name} /></label>
   <label class="text-sm">UID<input class={inputClass} inputmode="numeric" bind:value={user.uid} /></label>
   <label class="text-sm">GID<input class={inputClass} inputmode="numeric" bind:value={user.gid} /></label>
   <label class="text-sm">{t('svcHome')}<input class={inputClass} bind:value={user.home} /></label>
   <label class="text-sm">{t('svcPasswordSecret')}<input class={inputClass} bind:value={user.passwordSecret} /></label>
   {#if type === 'sftp'}<label class="text-sm">{t('svcKeysSecret')}<input class={inputClass} bind:value={user.authorizedKeysSecret} /></label>{/if}
   <label class="flex gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={user.readOnly} />{t('svcReadOnly')}</label>
   <Button type="button" variant="ghost" onclick={() => details.users = details.users.filter((_, j) => j !== i)}>{t('svcRouteRemove')}</Button>
  </div>
 {/each}
 <Button type="button" variant="outline" onclick={() => details.users = [...details.users, { name: '', uid: '1000', gid: '1000', home: '', readOnly: false, passwordSecret: '', authorizedKeysSecret: '' }]}>{t('svcAddUser')}</Button>
 </fieldset>
{/if}
{#if type === 'nfs'}
 <p class="text-sm text-destructive">{t('svcNfsBlocked')}</p>
 <fieldset class="space-y-3"><legend>{t('svcExports')}</legend>
 {#each details.exports as ex, i (i)}
  <div class="grid gap-2 rounded border p-3 md:grid-cols-3">
   <label class="text-sm">{t('svcExportPath')}<input class={inputClass} bind:value={ex.path} /></label>
   <label class="text-sm">{t('svcClients')}<input class={inputClass} bind:value={ex.clients} /></label>
   <label class="text-sm">{t('svcSquash')}<select class={selectClass} bind:value={ex.squash}><option>root_squash</option><option>all_squash</option><option>no_root_squash</option></select></label>
   <label class="flex gap-2 text-sm"><input type="checkbox" class={checkClass} bind:checked={ex.readOnly} />{t('svcReadOnly')}</label>
   <Button type="button" variant="ghost" onclick={() => details.exports = details.exports.filter((_, j) => j !== i)}>{t('svcRouteRemove')}</Button>
  </div>
 {/each}
 <Button type="button" variant="outline" onclick={() => details.exports = [...details.exports, { path: '/shared', clients: '', readOnly: false, squash: 'root_squash' }]}>{t('svcAddExport')}</Button>
 </fieldset>
{/if}
