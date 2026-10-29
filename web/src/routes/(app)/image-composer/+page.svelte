<script lang="ts">
	import { api, asApiError, type ApiError, type ComposerCatalog, type ComposerField, type ComposerPreview, type ImageBuildJob, type ImageBuildPlan } from '../../../lib/api';
	import {
		applicableFields,
		buildRecipe,
		currentValue,
		defaultForm,
		defaultText,
		featuresOn,
		issuesAt,
		localText,
		platformsOn,
		orderedComponents,
		reconcile,
		releasesOf,
		requiredBy,
		settingPath,
		switchComponent,
		versionsOn,
		type ComposerForm
	} from '../../../lib/composer';
	import { planStillValid, pushableRegistries } from '../../../lib/images';
	import { shortDigest } from '../../../lib/services';
	import { load } from '../../../lib/load.svelte';
	import { locale, t } from '../../../lib/locale.svelte';
	import { checkClass, selectClass, textareaClass } from '../../../lib/components/app/fieldClass';
	import * as Card from '../../../lib/components/ui/card/index.js';
	import * as Alert from '../../../lib/components/ui/alert/index.js';
	import { Button } from '../../../lib/components/ui/button/index.js';
	import { Input } from '../../../lib/components/ui/input/index.js';
	import { Label } from '../../../lib/components/ui/label/index.js';
	import PageHeader from '../../../lib/components/app/PageHeader.svelte';
	import ApiGate from '../../../lib/components/app/ApiGate.svelte';
	import Loading from '../../../lib/components/app/Loading.svelte';

	const catalog = load(api.composerCatalog);
	const registries = load(api.registries);
	const targets = $derived(registries.state.status === 'ok' ? pushableRegistries(registries.state.value) : []);

	let form = $state<ComposerForm | undefined>();
	$effect(() => {
		if (!form && catalog.state.status === 'ok') form = defaultForm(catalog.state.value);
	});

	let touched = $state(false);
	let preview = $state<ComposerPreview | undefined>();
	let previewBusy = $state(false);
	let previewError = $state<ApiError | undefined>();
	let shownFile = $state('Dockerfile');

	const lang = $derived(locale.value);
	const cat = $derived(catalog.state.status === 'ok' ? catalog.state.value : undefined);
	const recipe = $derived(cat && form ? buildRecipe(cat, form) : undefined);
	const recipeJson = $derived(JSON.stringify(recipe));

	// Live preview: the manager validates and generates; the page only asks (debounced, last answer wins).
	let generation = 0;
	$effect(() => {
		if (!recipe) return;
		const r = recipe;
		const g = ++generation;
		previewBusy = true;
		const timer = setTimeout(async () => {
			try {
				const p = await api.composerPreview(r);
				if (g !== generation) return;
				preview = p;
				previewError = undefined;
				if (shownFile !== 'Dockerfile' && !p.files?.some((f) => f.path === shownFile)) shownFile = 'Dockerfile';
			} catch (x) {
				if (g === generation) previewError = asApiError(x);
			} finally {
				if (g === generation) previewBusy = false;
			}
		}, 300);
		return () => clearTimeout(timer);
	});

	const issues = $derived(preview && !preview.valid ? preview.issues : []);
	const shownText = $derived(
		shownFile === 'Dockerfile' ? (preview?.dockerfile ?? '') : (preview?.files?.find((f) => f.path === shownFile)?.content ?? '')
	);

	function set(fn: (f: ComposerForm) => ComposerForm) {
		if (!cat || !form) return;
		touched = true;
		form = reconcile(cat, fn(form));
		plan = undefined;
	}
	const toggle = (list: string[], v: string, on: boolean) => (on ? [...new Set([...list, v])] : list.filter((x) => x !== v));

	function setSetting(compId: string, f: ComposerField, v: string | boolean) {
		set((cur) => ({ ...cur, components: { ...cur.components, [compId]: { ...cur.components[compId], settings: { ...cur.components[compId].settings, [f.key]: v } } } }));
	}
	function setComp(compId: string, patch: Partial<ComposerForm['components'][string]>) {
		if (cat && 'on' in patch && Object.keys(patch).length === 1) {
			set((cur) => switchComponent(cat, cur, compId, patch.on === true));
			return;
		}
		set((cur) => ({ ...cur, components: { ...cur.components, [compId]: { ...cur.components[compId], ...patch } } }));
	}
	function useBase(distro: string, release: string) {
		set((cur) => ({ ...cur, distro, release }));
	}

	const rel = $derived(cat && form ? releasesOf(cat, form.distro).find((r) => r.id === form!.release) : undefined);
	const platformChoices = $derived(cat && form ? platformsOn(cat, form) : []);
	const groups = (fields: ComposerField[]) => [...new Set(fields.map((f) => f.group ?? ''))];
	const names = (ids: string[]) => ids.map((id) => (cat?.components.find((c) => c.id === id) ? localText(cat.components.find((c) => c.id === id)!.name, lang) : id)).join(', ');
	const flatIssues = $derived(issues.filter((i) => !i.path.startsWith('components[') || !i.path.includes('.settings.')));

	// Build: plan, approval, build (the existing image build approval flow).
	let reg = $state('');
	let repository = $state('');
	let tag = $state('');
	let plan = $state<{ plan: ImageBuildPlan; request: string } | undefined>();
	let planBusy = $state(false);
	let planError = $state<ApiError | undefined>();
	let approved = $state(false);
	let building = $state(false);
	let buildError = $state<ApiError | undefined>();
	let job = $state<ImageBuildJob | undefined>();
	const buildRequest = $derived(recipe ? { recipe, target: { registry: reg, repository: repository.trim(), tag: tag.trim() } } : undefined);
	const buildJson = $derived(JSON.stringify(buildRequest));
	const stillValid = $derived(plan !== undefined && planStillValid(plan.request, buildJson));
	const canPlan = $derived(preview?.valid === true && reg !== '' && repository.trim() !== '' && tag.trim() !== '');

	async function makePlan() {
		if (!buildRequest || !canPlan) return;
		planBusy = true;
		planError = undefined;
		job = undefined;
		approved = false;
		try {
			const r = await api.composerPlan(buildRequest);
			plan = r.plan ? { plan: r.plan, request: buildJson } : undefined;
		} catch (x) {
			plan = undefined;
			planError = asApiError(x);
		} finally {
			planBusy = false;
		}
	}
	async function build() {
		if (!plan || !stillValid || !approved || !buildRequest) return;
		building = true;
		buildError = undefined;
		try {
			job = (await api.composerBuild(buildRequest, plan.plan.digest)).job;
		} catch (x) {
			buildError = asApiError(x);
		} finally {
			building = false;
		}
	}
	const fieldId = (compId: string, key: string) => `cmp-${compId}-${key.replace(/\W/g, '_')}`;
	const rangeText = (f: ComposerField) => t('cmpRange', { min: f.min ?? '', max: f.max ?? '' });
</script>

<PageHeader title={t('cmpTitle')} intro={t('cmpIntro')} />

{#if catalog.state.status === 'loading' || (catalog.state.status === 'ok' && !form)}
	<Loading />
{:else if catalog.state.status === 'error'}
	<ApiGate error={catalog.state.error} retry={catalog.reload} />
{:else if cat && form}
	<div class="space-y-4">
		<Alert.Root><Alert.Description>{t('cmpEvidence', { version: cat.version })}</Alert.Description></Alert.Root>

		<div class="grid min-w-0 gap-6 xl:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
			<!-- Selection -->
			<div class="min-w-0 space-y-4">
				<Card.Root>
					<Card.Header><Card.Title><h2>{t('cmpStepBase')}</h2></Card.Title></Card.Header>
					<Card.Content class="space-y-4">
						<div class="space-y-1.5">
							<Label for="cmp-name">{t('cmpName')}</Label>
							<Input id="cmp-name" value={form.name} oninput={(e) => set((c) => ({ ...c, name: e.currentTarget.value }))} autocomplete="off" aria-invalid={touched && issuesAt(issues, 'name').length > 0} aria-describedby="cmp-name-err" />
							<p id="cmp-name-err" class="text-xs text-red-700 dark:text-red-400">{touched ? issuesAt(issues, 'name').map((i) => i.message).join(' ') : ''}</p>
						</div>
						<div class="grid gap-4 sm:grid-cols-2">
							<div class="space-y-1.5">
								<Label for="cmp-distro">{t('cmpDistro')}</Label>
								<select id="cmp-distro" class={selectClass} value={form.distro} onchange={(e) => set((c) => ({ ...c, distro: e.currentTarget.value, release: '' }))}>
									{#each cat.distros as d (d.id)}<option value={d.id}>{d.name}</option>{/each}
								</select>
							</div>
							<div class="space-y-1.5">
								<Label for="cmp-release">{t('cmpRelease')}</Label>
								<select id="cmp-release" class={selectClass} value={form.release} onchange={(e) => set((c) => ({ ...c, release: e.currentTarget.value }))}>
									{#each releasesOf(cat, form.distro) as r (r.id)}<option value={r.id}>{r.name}</option>{/each}
								</select>
							</div>
						</div>
						<div class="space-y-1.5">
							<Label for="cmp-digest">{t('cmpDigest')}</Label>
							<Input id="cmp-digest" class="font-mono" value={form.digest} oninput={(e) => set((c) => ({ ...c, digest: e.currentTarget.value }))} autocomplete="off" placeholder={t('exampleSha')} aria-invalid={touched && issuesAt(issues, 'base.digest').length > 0} aria-describedby="cmp-digest-hint" />
							<p id="cmp-digest-hint" class="text-xs {touched && issuesAt(issues, 'base.digest').length > 0 ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">
								{touched && issuesAt(issues, 'base.digest').length > 0 ? issuesAt(issues, 'base.digest')[0].message : t('cmpDigestHint', { image: rel ? `${rel.image}:${rel.tag}` : '' })}
							</p>
						</div>
						<fieldset class="space-y-1.5">
							<legend class="text-sm font-medium">{t('cmpPlatforms')}</legend>
							<div class="flex flex-wrap gap-x-4 gap-y-1.5">
								{#each platformChoices as p (p)}
									<label class="flex items-center gap-1.5 text-sm"><input type="checkbox" class={checkClass} checked={form.platforms.includes(p)} onchange={(e) => set((c) => ({ ...c, platforms: toggle(c.platforms, p, e.currentTarget.checked) }))} /><span class="font-mono text-xs">{p}</span></label>
								{/each}
							</div>
						</fieldset>
					</Card.Content>
				</Card.Root>

				<h2 class="text-lg font-semibold">{t('cmpStepComponents')}</h2>
				{#each orderedComponents(cat) as comp, i (comp.id)}
					{@const c = form.components[comp.id]}
					{#if i === 0 || orderedComponents(cat)[i - 1].kind !== comp.kind}
						<p class="pt-2 text-sm font-medium text-muted-foreground">{t(comp.kind === 'server' ? 'cmpKindServer' : comp.kind === 'runtime' ? 'cmpKindRuntime' : comp.kind === 'tool' ? 'cmpKindTool' : 'cmpKindUtility')}</p>
					{/if}
					{@const avail = versionsOn(comp, form.distro, form.release)}
					{@const f0 = form}
					{@const blockers = (comp.requires ?? []).filter((r) => !f0.components[r]?.on)}
					{@const needed = requiredBy(cat, f0, comp.id)}
					{@const fields = applicableFields(comp, form)}
					{@const feats = featuresOn(cat, comp, form.distro)}
					<Card.Root>
						<Card.Header>
							<Card.Title>
								<h3 class="flex flex-wrap items-center gap-x-3 gap-y-1">
									<label class="flex items-center gap-2">
										<input
											type="checkbox"
											class={checkClass}
											checked={c.on}
											disabled={avail.length === 0 || blockers.length > 0 || (c.on && needed.length > 0)}
											onchange={(e) => setComp(comp.id, { on: e.currentTarget.checked })}
											aria-label={t('cmpInclude', { name: localText(comp.name, lang) })}
										/>
										{localText(comp.name, lang)}
									</label>
									<span class="text-xs font-normal text-muted-foreground">{comp.process === 'main' ? t('cmpProcessMain') : comp.process === 'companion' ? t('cmpProcessCompanion') : t('cmpProcessNone')}</span>
								</h3>
							</Card.Title>
							<p class="text-sm text-muted-foreground">{localText(comp.description, lang)}</p>
							{#if avail.length === 0}<p class="text-xs text-muted-foreground">{t('cmpNotOnRelease')}</p>{/if}
							{#if blockers.length > 0}<p class="text-xs text-muted-foreground">{t('cmpNeeds', { names: names(blockers) })}</p>{/if}
							{#if needed.length > 0}<p class="text-xs text-muted-foreground">{t('cmpNeededBy', { names: names(needed) })}</p>{/if}
						</Card.Header>
						{#if c.on}
							<Card.Content class="space-y-4">
								<div class="max-w-xs space-y-1.5">
									<Label for={fieldId(comp.id, 'version')}>{t('cmpVersion')}</Label>
									<select id={fieldId(comp.id, 'version')} class={selectClass} value={c.version} onchange={(e) => setComp(comp.id, { version: e.currentTarget.value })}>
										{#each avail as v (v)}<option value={v}>{v}</option>{/each}
									</select>
								</div>
								{#if feats.length > 0}
									<fieldset class="space-y-1.5">
										<legend class="text-sm font-medium">{localText(comp.featureLabel, lang)}</legend>
										<div class="flex flex-wrap gap-x-4 gap-y-1.5">
											{#each feats as f (f)}
												<label class="flex items-center gap-1.5 text-sm"><input type="checkbox" class={checkClass} checked={c.features.includes(f)} onchange={(e) => setComp(comp.id, { features: toggle(c.features, f, e.currentTarget.checked) })} /><span class="font-mono text-xs">{f}</span></label>
											{/each}
										</div>
										{#if issuesAt(issues, `components[${comp.id}].features`).length > 0}
											<p class="text-xs text-red-700 dark:text-red-400">{issuesAt(issues, `components[${comp.id}].features`)[0].message}</p>
										{/if}
									</fieldset>
								{/if}
								{#if fields.length > 0}
									<div class="space-y-1">
										<h4 class="text-sm font-medium">{t('cmpSettings')}</h4>
										<p class="text-xs text-muted-foreground">{t('cmpSettingsHint')}</p>
									</div>
									{#each groups(fields) as g (g)}
										<fieldset class="space-y-3 rounded-md border p-3">
											{#if g}<legend class="px-1 text-xs font-medium text-muted-foreground">{g}</legend>{/if}
											<div class="grid gap-x-4 gap-y-3 sm:grid-cols-2">
												{#each fields.filter((f) => (f.group ?? '') === g) as f (f.key)}
													{@const id = fieldId(comp.id, f.key)}
													{@const problems = issuesAt(issues, settingPath(comp.id, f.key))}
													{@const val = currentValue(f, c)}
													<div class="min-w-0 space-y-1">
														{#if f.type === 'bool'}
															<label class="flex items-start gap-2 text-sm"><input {id} type="checkbox" class="{checkClass} mt-0.5" checked={val === true} onchange={(e) => setSetting(comp.id, f, e.currentTarget.checked)} aria-describedby="{id}-d" />{localText(f.label, lang)}</label>
														{:else}
															<Label for={id} class="text-xs">{localText(f.label, lang)}</Label>
															{#if f.type === 'enum'}
																<select {id} class={selectClass} value={String(val)} onchange={(e) => setSetting(comp.id, f, e.currentTarget.value)} aria-describedby="{id}-d">
																	{#each f.enum ?? [] as o (o.value)}<option value={o.value}>{o.value}</option>{/each}
																</select>
															{:else if f.multiline}
																<textarea {id} class={textareaClass} rows="6" value={String(val)} oninput={(e) => setSetting(comp.id, f, e.currentTarget.value)} autocomplete="off" spellcheck="false" aria-invalid={problems.length > 0} aria-describedby="{id}-d"></textarea>
															{:else}
																<Input {id} class="font-mono" inputmode={f.type === 'int' ? 'numeric' : undefined} value={String(val)} oninput={(e) => setSetting(comp.id, f, e.currentTarget.value)} autocomplete="off" aria-invalid={problems.length > 0} aria-describedby="{id}-d" />
															{/if}
														{/if}
														<p id="{id}-d" class="text-xs {problems.length > 0 ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'}">
															{#if problems.length > 0}{problems.map((p) => p.message).join(' ')}{:else}
																{f.help ? localText(f.help, lang) + ' ' : ''}{f.type === 'int' ? rangeText(f) + '. ' : ''}{t('cmpDefaultIs', { value: f.type === 'bool' ? (f.default ? t('cmpOn') : t('cmpOff')) : defaultText(f) })}
															{/if}
														</p>
													</div>
												{/each}
											</div>
										</fieldset>
									{/each}
								{/if}
							</Card.Content>
						{/if}
					</Card.Root>
				{/each}
			</div>

			<!-- Review -->
			<div class="min-w-0 space-y-4 xl:sticky xl:top-20 xl:self-start">
				<Card.Root>
					<Card.Header><Card.Title><h2>{t('cmpPreviewTitle')}</h2></Card.Title></Card.Header>
					<Card.Content class="space-y-4">
						<div aria-live="polite" class="text-sm">
							{#if previewError}
								<ApiGate error={previewError} />
							{:else if previewBusy && !preview}
								<p class="text-muted-foreground">{t('cmpChecking')}</p>
							{:else if preview?.valid}
								<p class="font-medium text-emerald-700 dark:text-emerald-400">{t('cmpValid')}</p>
							{:else if preview && touched}
								<p class="font-medium text-red-700 dark:text-red-400">{t('cmpInvalid')}</p>
							{/if}
						</div>
						{#if touched && flatIssues.length > 0}
							<section aria-labelledby="cmp-issues" class="space-y-1">
								<h3 id="cmp-issues" class="text-sm font-medium">{t('cmpIssues')}</h3>
								<ul class="list-disc space-y-0.5 pl-5 text-sm">
									{#each flatIssues as i (i.path + i.message)}<li><code class="font-mono text-xs break-all">{i.path}</code> {i.message}</li>{/each}
								</ul>
							</section>
						{/if}

						{#if preview}
							<section aria-labelledby="cmp-variants" class="space-y-2">
								<h3 id="cmp-variants" class="text-sm font-medium">{t('cmpVariants')}</h3>
								<p class="text-xs text-muted-foreground">{t('cmpVariantsHint')}</p>
								<ul class="space-y-1.5">
									{#each preview.variants as v (v.distro + v.release)}
										{@const current = v.distro === form.distro && v.release === form.release}
										<li class="rounded-md border p-2 text-sm">
											<div class="flex flex-wrap items-center gap-2">
												<span class="min-w-0 flex-1 font-medium">{v.name}</span>
												<span class={v.compatible ? 'text-xs font-medium text-emerald-700 dark:text-emerald-400' : 'text-xs font-medium text-amber-700 dark:text-amber-400'}>{v.compatible ? t('cmpVariantOk') : t('cmpVariantNo')}</span>
												{#if current}
													<span class="text-xs text-muted-foreground">{t('cmpVariantCurrent')}</span>
												{:else}
													<Button size="sm" variant="outline" onclick={() => useBase(v.distro, v.release)}>{t('cmpVariantSwitch')}</Button>
												{/if}
											</div>
											{#if v.problems && v.problems.length > 0}<ul class="mt-1 list-disc pl-5 text-xs text-muted-foreground">{#each v.problems as p (p)}<li>{p}</li>{/each}</ul>{/if}
										</li>
									{/each}
								</ul>
							</section>
						{/if}

						{#if preview?.valid && preview.dockerfile}
							<section aria-labelledby="cmp-files" class="space-y-2">
								<h3 id="cmp-files" class="text-sm font-medium">{t('cmpFiles')}</h3>
								<div class="space-y-1">
									<Label for="cmp-file" class="text-xs">{t('cmpChooseFile')}</Label>
									<select id="cmp-file" class={selectClass} bind:value={shownFile}>
										<option value="Dockerfile">{t('cmpDockerfile')}</option>
										{#each preview.files ?? [] as f (f.path)}<option value={f.path}>{f.path}</option>{/each}
									</select>
								</div>
								<!-- A scrollable code region must be focusable so keyboard users can scroll it. -->
								<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
								<div role="region" tabindex="0" aria-label={shownFile} class="max-h-96 overflow-auto rounded-md border bg-muted/40 p-2 outline-none focus-visible:ring-2 focus-visible:ring-ring">
									<pre class="font-mono text-xs"><code>{shownText}</code></pre>
								</div>
								<p class="text-xs">{t('cmpRecipeDigest')}: <code class="font-mono break-all">{preview.digest}</code></p>
								<p class="text-xs">{t('cmpContextHash')}: <code class="font-mono break-all">{preview.contextHash}</code></p>
							</section>
							{#if preview.packages && preview.packages.length > 0}
								<section aria-labelledby="cmp-pkgs" class="space-y-1">
									<h3 id="cmp-pkgs" class="text-sm font-medium">{t('cmpPackages')}</h3>
									<p class="font-mono text-xs break-words">{preview.packages.map((p) => p.name + (p.constraint ? '~' + p.constraint : '')).join(', ')}</p>
								</section>
							{/if}
							{#if preview.warnings && preview.warnings.length > 0}
								<Alert.Root>
									<Alert.Title>{t('cmpWarnings')}</Alert.Title>
									<Alert.Description><ul class="list-disc pl-5">{#each preview.warnings as w (w)}<li>{w}</li>{/each}</ul></Alert.Description>
								</Alert.Root>
							{/if}
							{#if preview.notes && preview.notes.length > 0}
								<section aria-labelledby="cmp-notes" class="space-y-1">
									<h3 id="cmp-notes" class="text-sm font-medium">{t('cmpNotes')}</h3>
									<ul class="list-disc space-y-0.5 pl-5 text-xs text-muted-foreground">{#each preview.notes as n (n)}<li>{n}</li>{/each}</ul>
								</section>
							{/if}
						{/if}
					</Card.Content>
				</Card.Root>

				<Card.Root>
					<Card.Header><Card.Title><h2>{t('imgTarget')}</h2></Card.Title></Card.Header>
					<Card.Content class="space-y-4">
						<p class="text-xs text-muted-foreground">{t('cmpPlanHint')}</p>
						<div class="grid gap-3 sm:grid-cols-3">
							<div class="space-y-1">
								<Label for="cmp-reg" class="text-xs">{t('imgRegistry')}</Label>
								<select id="cmp-reg" class={selectClass} bind:value={reg}>
									<option value="">{t('choose')}</option>
									{#each targets as r (r.id)}<option value={r.id}>{r.name}</option>{/each}
								</select>
								{#if registries.state.status === 'ok' && targets.length === 0}<p class="text-xs text-muted-foreground">{t('imgRegistryNone')}</p>{/if}
							</div>
							<div class="space-y-1">
								<Label for="cmp-repo" class="text-xs">{t('imgRepository')}</Label>
								<Input id="cmp-repo" class="font-mono" bind:value={repository} autocomplete="off" placeholder={t('exampleRepository')} />
							</div>
							<div class="space-y-1">
								<Label for="cmp-tag" class="text-xs">{t('imgTag')}</Label>
								<Input id="cmp-tag" class="font-mono" bind:value={tag} autocomplete="off" placeholder={t('exampleTag')} />
							</div>
						</div>
						<Button disabled={planBusy || !canPlan} onclick={makePlan}>{t('cmpPlanBtn')}</Button>
						<div aria-live="polite" class="space-y-3">
							{#if planError}<ApiGate error={planError} />{/if}
							{#if plan}
								{@const p = plan.plan}
								<section class="space-y-3 rounded-md border p-3 text-sm" aria-label={t('cmpPlanTitle')}>
									<h3 class="font-medium">{t('cmpPlanTitle')}</h3>
									<p class="whitespace-pre-line">{p.explanation}</p>
									<ol class="list-decimal space-y-0.5 pl-5">{#each p.steps as s (s.order ?? s.title)}<li>{s.title}{#if s.detail}<span class="text-muted-foreground">{' — '}{s.detail}</span>{/if}</li>{/each}</ol>
									{#if p.warnings.length > 0}
										<Alert.Root>
											<Alert.Title>{t('imgPlanWarnings')}</Alert.Title>
											<Alert.Description><ul class="list-disc pl-5">{#each p.warnings as w (w)}<li>{w}</li>{/each}</ul></Alert.Description>
										</Alert.Root>
									{/if}
									<p class="text-xs">{t('imgPlanDigest')}: <code class="font-mono break-all">{p.digest}</code></p>
									{#if !stillValid}
										<Alert.Root><Alert.Description>{t('imgPlanStale')}</Alert.Description></Alert.Root>
									{:else if !job}
										<label class="flex items-start gap-2"><input type="checkbox" class="{checkClass} mt-0.5" bind:checked={approved} />{t('imgApprove', { digest: shortDigest(p.digest) })}</label>
										<Button disabled={building || !approved} onclick={build}>{t('cmpBuildBtn')}</Button>
									{/if}
									{#if buildError}<ApiGate error={buildError} />{/if}
									{#if job}<Alert.Root><Alert.Description>{t('imgBuildStarted', { id: job.id, status: t(job.status === 'succeeded' ? 'imgStatusSucceeded' : job.status === 'failed' ? 'imgStatusFailed' : 'imgStatusRunning') })}</Alert.Description></Alert.Root>{/if}
								</section>
							{/if}
						</div>
					</Card.Content>
				</Card.Root>
			</div>
		</div>
	</div>
{/if}
