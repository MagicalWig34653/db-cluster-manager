<script lang="ts">
	import { api, asApiError, type ApiError, type RegistryKind } from '../../../lib/api';
	import { buildRegistryInput, emptyRegistryForm, validateRegistryForm, type RegistryFormError } from '../../../lib/images';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import { selectClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const registries = load(api.registries);
	const kindLabel: Record<RegistryKind, MessageKey> = {
		ghcr: 'regKindGhcr',
		dockerhub: 'regKindDockerhub',
		generic: 'regKindGeneric',
		managed: 'regKindManaged'
	};

	let form = $state(emptyRegistryForm());
	let invalid = $state<RegistryFormError[]>([]);
	let busy = $state(false);
	let error = $state<ApiError | undefined>();
	let added = $state<string | undefined>();

	async function add(e: SubmitEvent) {
		e.preventDefault();
		invalid = validateRegistryForm(form);
		added = undefined;
		if (invalid.length > 0) return;
		busy = true;
		error = undefined;
		try {
			const r = await api.createRegistry(buildRegistryInput(form));
			added = r.name;
			// The password is write-only: drop it from the page as soon as it was sent.
			form = emptyRegistryForm();
			registries.refresh();
		} catch (x) {
			error = asApiError(x);
			form.password = '';
		} finally {
			busy = false;
		}
	}
</script>

<PageHeader title={t('regTitle')} intro={t('regIntro')} />

<div class="space-y-6">
	{#if registries.state.status === 'loading'}
		<Loading />
	{:else if registries.state.status === 'error'}
		<ApiGate error={registries.state.error} retry={registries.reload} />
	{:else if registries.state.value.length === 0}
		<p class="text-sm text-muted-foreground">{t('regEmpty')}</p>
	{:else}
		<div class="rounded-lg border">
			<Table.Root>
				<Table.Header>
					<Table.Row>
						<Table.Head>{t('regColName')}</Table.Head>
						<Table.Head>{t('regColKind')}</Table.Head>
						<Table.Head>{t('regColEndpoint')}</Table.Head>
						<Table.Head>{t('regColUser')}</Table.Head>
						<Table.Head>{t('regColCredential')}</Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each registries.state.value as r (r.id)}
						<Table.Row>
							<Table.Cell class="font-medium">{r.name}<span class="ml-1 font-mono text-xs text-muted-foreground">({r.id})</span></Table.Cell>
							<Table.Cell>{t(kindLabel[r.kind])}</Table.Cell>
							<Table.Cell class="font-mono text-xs">{r.endpoint ?? t('notReported')}</Table.Cell>
							<Table.Cell class="font-mono text-xs">{r.username ?? '—'}</Table.Cell>
							<Table.Cell>{r.credentialSet ? t('regCredentialSet') : t('regCredentialNone')}</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
	{/if}

	<Card.Root>
		<Card.Header><Card.Title><h2>{t('regAdd')}</h2></Card.Title></Card.Header>
		<Card.Content>
			<form class="max-w-2xl space-y-4" onsubmit={add} aria-label={t('regAdd')} autocomplete="off">
				<div class="grid gap-4 md:grid-cols-2">
					<div class="space-y-1.5">
						<Label for="rg-id">{t('regId')}</Label>
						<Input id="rg-id" bind:value={form.id} autocomplete="off" aria-invalid={invalid.includes('id')} aria-describedby="rg-id-hint" />
						<p id="rg-id-hint" class="text-xs {invalid.includes('id') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('id') ? t('valRegId') : t('regIdHint')}</p>
					</div>
					<div class="space-y-1.5">
						<Label for="rg-name">{t('regName')}</Label>
						<Input id="rg-name" bind:value={form.name} autocomplete="off" aria-invalid={invalid.includes('name')} />
						{#if invalid.includes('name')}<p class="text-xs text-red-700 dark:text-red-400">{t('valRegName')}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="rg-kind">{t('regKind')}</Label>
						<select id="rg-kind" class={selectClass} bind:value={form.kind}>
							<option value="ghcr">{t('regKindGhcr')}</option>
							<option value="dockerhub">{t('regKindDockerhub')}</option>
							<option value="generic">{t('regKindGeneric')}</option>
						</select>
					</div>
					{#if form.kind === 'generic'}
						<div class="space-y-1.5">
							<Label for="rg-ep">{t('regEndpoint')}</Label>
							<Input id="rg-ep" class="font-mono" bind:value={form.endpoint} autocomplete="off" placeholder="https://registry.example.org" aria-invalid={invalid.includes('endpoint')} aria-describedby="rg-ep-hint" />
							<p id="rg-ep-hint" class="text-xs {invalid.includes('endpoint') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{invalid.includes('endpoint') ? t('valRegEndpoint') : t('regEndpointHint')}</p>
						</div>
					{/if}
					<div class="space-y-1.5">
						<Label for="rg-user">{t('regUser')}</Label>
						<Input id="rg-user" bind:value={form.username} autocomplete="off" />
					</div>
					<div class="space-y-1.5">
						<Label for="rg-pw">{t('regPassword')}</Label>
						<Input id="rg-pw" type="password" bind:value={form.password} autocomplete="new-password" aria-describedby="rg-pw-hint" />
					</div>
				</div>
				<p id="rg-pw-hint" class="text-xs text-muted-foreground">{t('regPasswordHint')}</p>
				<Button type="submit" disabled={busy}>{t('regSave')}</Button>
				<div aria-live="polite" class="space-y-2">
					{#if error}<ApiGate {error} />{/if}
					{#if added}<Alert.Root><Alert.Description>{t('regAdded', { name: added })}</Alert.Description></Alert.Root>{/if}
				</div>
			</form>
		</Card.Content>
	</Card.Root>
</div>
