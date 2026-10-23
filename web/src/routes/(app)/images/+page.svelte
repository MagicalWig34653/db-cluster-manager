<script lang="ts">
	import { api, asApiError, type ApiError, type BuildStatus, type ImageBuildJob, type ImageBuildPlan } from '../../../lib/api';
	import {
		buildImageRequest,
		buildOrder,
		emptyImageForm,
		extensionsFor,
		packageCatalog,
		phpVersions,
		nginxVersions,
		platformChoices,
		planStillValid,
		pruneExtensions,
		pushableRegistries,
		validateImageForm,
		type ImageFormError
	} from '../../../lib/images';
	import { shortDigest } from '../../../lib/services';
	import { load } from '../../../lib/load.svelte';
	import { t } from '../../../lib/locale.svelte';
	import type { MessageKey } from '../../../lib/i18n';
	import { checkClass, selectClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Table from '../../../lib/components/ui/table/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import Freshness from '../../../lib/components/app/Freshness.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const builds = load(api.imageBuilds);
	const registries = load(api.registries);
	const targets = $derived(registries.state.status === 'ok' ? pushableRegistries(registries.state.value) : []);

	let form = $state(emptyImageForm());
	let invalid = $state<ImageFormError[]>([]);
	let shown = $state<{ plan: ImageBuildPlan; request: string } | undefined>();
	let planBusy = $state(false);
	let planError = $state<ApiError | undefined>();
	let approved = $state(false);
	let building = $state(false);
	let buildError = $state<ApiError | undefined>();
	let job = $state<ImageBuildJob | undefined>();

	const request = $derived(buildImageRequest(form));
	const requestJson = $derived(JSON.stringify(request));
	const valid = $derived(shown !== undefined && planStillValid(shown.request, requestJson));
	const exts = $derived(extensionsFor(form.phpVersion));

	const statusLabel: Record<BuildStatus, MessageKey> = { running: 'imgStatusRunning', succeeded: 'imgStatusSucceeded', failed: 'imgStatusFailed' };
	const msg = (k: ImageFormError, key: MessageKey) => (invalid.includes(k) ? t(key) : undefined);
	const bad = (...k: ImageFormError[]) => k.some((x) => invalid.includes(x));
	const toggle = (list: string[], v: string, on: boolean) => (on ? [...new Set([...list, v])] : list.filter((x) => x !== v));

	async function showPlan(e: SubmitEvent) {
		e.preventDefault();
		invalid = validateImageForm(form);
		job = undefined;
		approved = false;
		if (invalid.length > 0) return;
		planBusy = true;
		planError = undefined;
		try {
			const r = await api.planImageBuild(request);
			shown = r.plan ? { plan: r.plan, request: requestJson } : undefined;
		} catch (x) {
			shown = undefined;
			planError = asApiError(x);
		} finally {
			planBusy = false;
		}
	}
	async function build() {
		if (!shown || !valid || !approved) return;
		building = true;
		buildError = undefined;
		try {
			const r = await api.startImageBuild(request, shown.plan.digest);
			job = r.job;
			builds.refresh();
		} catch (x) {
			buildError = asApiError(x);
		} finally {
			building = false;
		}
	}
</script>

<PageHeader title={t('imgTitle')} intro={t('imgIntro')} />

<div class="space-y-6">
	<Alert.Root><Alert.Description>{t('imgLimitNote')}</Alert.Description></Alert.Root>

	<section class="space-y-2" aria-labelledby="img-builds">
		<h2 id="img-builds" class="text-lg font-semibold">{t('imgBuilds')}</h2>
		{#if builds.state.status === 'loading'}
			<Loading />
		{:else if builds.state.status === 'error'}
			<ApiGate error={builds.state.error} retry={builds.reload} />
		{:else if builds.state.value.length === 0}
			<p class="text-sm text-muted-foreground">{t('imgBuildsEmpty')}</p>
		{:else}
			<div class="rounded-lg border">
				<Table.Root>
					<Table.Header>
						<Table.Row>
							<Table.Head>{t('imgColBuild')}</Table.Head>
							<Table.Head>{t('imgColStatus')}</Table.Head>
							<Table.Head>{t('imgColReference')}</Table.Head>
							<Table.Head>{t('imgColDigest')}</Table.Head>
							<Table.Head>{t('imgColSbom')}</Table.Head>
							<Table.Head>{t('imgColWhen')}</Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each buildOrder(builds.state.value) as j (j.id)}
							<Table.Row>
								<Table.Cell class="font-medium">{j.name ?? j.id}</Table.Cell>
								<Table.Cell>
									<span class={j.status === 'failed' ? 'font-medium text-red-700 dark:text-red-400' : j.status === 'succeeded' ? 'font-medium text-emerald-700 dark:text-emerald-400' : 'font-medium'}>{t(statusLabel[j.status])}</span>
									{#if j.error}<p class="max-w-xs text-xs text-muted-foreground">{j.error}</p>{/if}
								</Table.Cell>
								<Table.Cell class="font-mono text-xs break-all">{j.reference}</Table.Cell>
								<Table.Cell class="font-mono text-xs">{j.imageDigest ? shortDigest(j.imageDigest) : t('stateUnknown')}</Table.Cell>
								<Table.Cell class="text-xs">{j.sbom.status === 'not_generated' || !j.sbom.status ? t('imgSbomNone') : j.sbom.status}</Table.Cell>
								<Table.Cell><Freshness at={j.createdAt} /></Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			</div>
		{/if}
	</section>

	<Card.Root>
		<Card.Header><Card.Title><h2>{t('imgComposer')}</h2></Card.Title></Card.Header>
		<Card.Content>
			<form class="space-y-5" onsubmit={showPlan} aria-label={t('imgComposer')}>
				<div class="grid gap-4 md:grid-cols-3">
					<div class="space-y-1.5">
						<Label for="im-name">{t('imgName')}</Label>
						<Input id="im-name" bind:value={form.name} autocomplete="off" aria-invalid={bad('name')} />
						{#if msg('name', 'valImgName')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgName')}</p>{/if}
					</div>
					<div class="space-y-1.5">
						<Label for="im-nginx">{t('imgNginx')}</Label>
						<select id="im-nginx" class={selectClass} bind:value={form.nginxVersion}>{#each nginxVersions as v (v)}<option value={v}>nginx {v}</option>{/each}</select>
					</div>
					<div class="space-y-1.5">
						<Label for="im-php">{t('imgPhp')}</Label>
						<select id="im-php" class={selectClass} bind:value={form.phpVersion} onchange={() => (form.extensions = pruneExtensions(form.phpVersion, form.extensions))}>
							<option value="">{t('imgPhpNone')}</option>
							{#each phpVersions as v (v)}<option value={v}>PHP {v}</option>{/each}
						</select>
					</div>
				</div>
				<div class="space-y-1.5">
					<Label for="im-digest">{t('imgBaseDigest')}</Label>
					<Input id="im-digest" class="font-mono" bind:value={form.baseDigest} autocomplete="off" placeholder="sha256:…" aria-invalid={bad('digest')} aria-describedby="im-digest-hint" />
					<p id="im-digest-hint" class="text-xs {bad('digest') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{bad('digest') ? t('valImgDigest') : t('imgBaseDigestHint')}</p>
				</div>

				{#if form.phpVersion !== ''}
					<fieldset class="space-y-2">
						<legend class="text-sm font-medium">{t('imgExtensions')}</legend>
						<p class="text-xs text-muted-foreground">{t('imgExtensionsHint')}</p>
						<div class="flex flex-wrap gap-x-4 gap-y-1.5">
							{#each exts as e (e)}
								<label class="flex items-center gap-1.5 text-sm"><input type="checkbox" class={checkClass} checked={form.extensions.includes(e)} onchange={(ev) => (form.extensions = toggle(form.extensions, e, ev.currentTarget.checked))} /><span class="font-mono text-xs">{e}</span></label>
							{/each}
						</div>
					</fieldset>
					<div class="space-y-1.5">
						<Label for="im-ini">{t('imgIni')}</Label>
						<textarea id="im-ini" rows="3" class="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring" bind:value={form.ini} aria-invalid={bad('ini')} aria-describedby="im-ini-hint"></textarea>
						<p id="im-ini-hint" class="text-xs {bad('ini') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{bad('ini') ? t('valImgIni') : t('imgIniHint')}</p>
					</div>
				{:else if bad('iniWithoutPhp')}
					<p class="text-xs text-red-700 dark:text-red-400">{t('valImgIniNoPhp')}</p>
				{/if}

				<fieldset class="space-y-3">
					<legend class="text-sm font-medium">{t('imgNginxOptions')}</legend>
					<div class="grid gap-3 md:grid-cols-4">
						<div class="space-y-1">
							<Label for="im-wp" class="text-xs">{t('imgWorkers')}</Label>
							<Input id="im-wp" bind:value={form.workerProcesses} aria-invalid={bad('workerProcesses')} />
							{#if bad('workerProcesses')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgWorkers')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-wc" class="text-xs">{t('imgConnections')}</Label>
							<Input id="im-wc" inputmode="numeric" bind:value={form.workerConnections} aria-invalid={bad('workerConnections')} />
							{#if bad('workerConnections')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgConnections')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-sn" class="text-xs">{t('imgServerName')}</Label>
							<Input id="im-sn" bind:value={form.serverName} aria-invalid={bad('serverName')} />
							{#if bad('serverName')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgServerName')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-root" class="text-xs">{t('imgRoot')}</Label>
							<Input id="im-root" class="font-mono" bind:value={form.documentRoot} aria-invalid={bad('documentRoot')} />
							{#if bad('documentRoot')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgRoot')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-body" class="text-xs">{t('imgBodySize')}</Label>
							<Input id="im-body" bind:value={form.clientMaxBodySize} aria-invalid={bad('bodySize')} />
							{#if bad('bodySize')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgBodySize')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-ka" class="text-xs">{t('imgKeepalive')}</Label>
							<Input id="im-ka" inputmode="numeric" bind:value={form.keepaliveTimeout} aria-invalid={bad('keepalive')} />
							{#if bad('keepalive')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgKeepalive')}</p>{/if}
						</div>
						{#if form.phpVersion !== ''}
							<div class="space-y-1">
								<Label for="im-fc" class="text-xs">{t('imgFastcgi')}</Label>
								<Input id="im-fc" inputmode="numeric" bind:value={form.fastcgiReadTimeout} aria-invalid={bad('fastcgi')} />
								{#if bad('fastcgi')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgFastcgi')}</p>{/if}
							</div>
						{/if}
					</div>
					<div class="flex flex-wrap gap-x-5 gap-y-1.5 text-sm">
						<label class="flex items-center gap-2"><input type="checkbox" class={checkClass} bind:checked={form.gzip} />{t('imgGzip')}</label>
						<label class="flex items-center gap-2"><input type="checkbox" class={checkClass} bind:checked={form.securityHeaders} />{t('imgHeaders')}</label>
						<label class="flex items-center gap-2"><input type="checkbox" class={checkClass} bind:checked={form.serverTokens} />{t('imgTokens')}</label>
					</div>
				</fieldset>

				<div class="space-y-1.5">
					<Label for="im-snip">{t('imgSnippet')}</Label>
					<textarea id="im-snip" rows="4" class="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring" bind:value={form.snippet} aria-invalid={bad('snippet')} aria-describedby="im-snip-hint" placeholder={'location /health {\n    return 200 "ok";\n}'}></textarea>
					<p id="im-snip-hint" class="text-xs {bad('snippet') ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">{bad('snippet') ? t('valImgSnippet') : t('imgSnippetHint')}</p>
				</div>

				<div class="grid gap-4 md:grid-cols-2">
					<fieldset class="space-y-2">
						<legend class="text-sm font-medium">{t('imgPackages')}</legend>
						<div class="flex flex-wrap gap-x-4 gap-y-1.5">
							{#each packageCatalog as p (p)}
								<label class="flex items-center gap-1.5 text-sm"><input type="checkbox" class={checkClass} checked={form.packages.includes(p)} onchange={(ev) => (form.packages = toggle(form.packages, p, ev.currentTarget.checked))} /><span class="font-mono text-xs">{p}</span></label>
							{/each}
						</div>
					</fieldset>
					<fieldset class="space-y-2">
						<legend class="text-sm font-medium">{t('imgPlatforms')}</legend>
						<div class="flex flex-wrap gap-x-4 gap-y-1.5">
							{#each platformChoices as p (p)}
								<label class="flex items-center gap-1.5 text-sm"><input type="checkbox" class={checkClass} checked={form.platforms.includes(p)} onchange={(ev) => (form.platforms = toggle(form.platforms, p, ev.currentTarget.checked))} /><span class="font-mono text-xs">{p}</span></label>
							{/each}
						</div>
					</fieldset>
				</div>

				<fieldset class="space-y-2">
					<legend class="text-sm font-medium">{t('imgTarget')}</legend>
					<div class="grid gap-3 md:grid-cols-3">
						<div class="space-y-1">
							<Label for="im-reg" class="text-xs">{t('imgRegistry')}</Label>
							<select id="im-reg" class={selectClass} bind:value={form.registry} aria-invalid={bad('registry')}>
								<option value="">{t('choose')}</option>
								{#each targets as r (r.id)}<option value={r.id}>{r.name}</option>{/each}
							</select>
							{#if registries.state.status === 'ok' && targets.length === 0}<p class="text-xs text-muted-foreground">{t('imgRegistryNone')}</p>{:else if bad('registry')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgRegistry')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-repo" class="text-xs">{t('imgRepository')}</Label>
							<Input id="im-repo" class="font-mono" bind:value={form.repository} autocomplete="off" placeholder="team/shop-web" aria-invalid={bad('repository')} />
							{#if bad('repository')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgRepository')}</p>{/if}
						</div>
						<div class="space-y-1">
							<Label for="im-tag" class="text-xs">{t('imgTag')}</Label>
							<Input id="im-tag" class="font-mono" bind:value={form.tag} autocomplete="off" placeholder="1.0.0" aria-invalid={bad('tag')} />
							{#if bad('tag')}<p class="text-xs text-red-700 dark:text-red-400">{t('valImgTag')}</p>{/if}
						</div>
					</div>
				</fieldset>

				<Button type="submit" disabled={planBusy}>{t('imgPlanBtn')}</Button>
			</form>

			<div aria-live="polite" class="mt-4 space-y-3">
				{#if planError}<ApiGate error={planError} />{/if}
				{#if shown}
					{@const p = shown.plan}
					<section class="space-y-3 rounded-md border p-3 text-sm" aria-label={t('imgPlanTitle')}>
						<h3 class="font-medium">{t('imgPlanTitle')}</h3>
						<p class="text-muted-foreground">{t('imgPlanIntro')}</p>
						<p class="whitespace-pre-line">{p.explanation}</p>
						<div>
							<h4 class="mb-1 font-medium">{t('imgDockerfile')}</h4>
							<pre class="max-h-80 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-xs"><code>{p.dockerfile}</code></pre>
						</div>
						{#if p.files && p.files.length > 0}
							<div>
								<h4 class="mb-1 font-medium">{t('imgContextFiles')}</h4>
								<ul class="list-disc pl-5 font-mono text-xs">{#each p.files as f (f.path)}<li>{f.path}</li>{/each}</ul>
							</div>
						{/if}
						<div>
							<h4 class="mb-1 font-medium">{t('imgPlanSteps')}</h4>
							<ol class="list-decimal space-y-0.5 pl-5">{#each p.steps as s (s.order ?? s.title)}<li>{s.title}{#if s.detail}<span class="text-muted-foreground">{" — "}{s.detail}</span>{/if}</li>{/each}</ol>
						</div>
						{#if p.warnings.length > 0}
							<Alert.Root>
								<Alert.Title>{t('imgPlanWarnings')}</Alert.Title>
								<Alert.Description><ul class="list-disc pl-5">{#each p.warnings as w (w)}<li>{w}</li>{/each}</ul></Alert.Description>
							</Alert.Root>
						{/if}
						<p class="text-xs">{t('imgPlanDigest')}: <code class="font-mono break-all">{p.digest}</code></p>
						{#if !valid}
							<Alert.Root><Alert.Description>{t('imgPlanStale')}</Alert.Description></Alert.Root>
						{:else if !job}
							<label class="flex items-start gap-2"><input type="checkbox" class="{checkClass} mt-0.5" bind:checked={approved} />{t('imgApprove', { digest: shortDigest(p.digest) })}</label>
							<Button disabled={building || !approved} onclick={build}>{t('imgBuildBtn')}</Button>
						{/if}
						{#if buildError}<ApiGate error={buildError} />{/if}
						{#if job}<Alert.Root><Alert.Description>{t('imgBuildStarted', { id: job.id, status: t(statusLabel[job.status]) })}</Alert.Description></Alert.Root>{/if}
					</section>
				{/if}
			</div>
		</Card.Content>
	</Card.Root>
</div>
