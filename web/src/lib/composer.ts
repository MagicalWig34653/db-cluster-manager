// Pure rules of the image composer page (X17, ADR 0026). The catalog comes
// from GET /image-composer/catalog; this module only turns it into form
// choices and a recipe. It decides nothing about validity: the manager
// validates every recipe and its findings are shown as they arrive.

import type { ComposerCatalog, ComposerComponent, ComposerField, ComposerIssue, ComposerRecipe, ComposerText } from './api';

export type Locale = 'en' | 'de';

/** Picks the text of the UI language. */
export const localText = (x: ComposerText | undefined, locale: Locale): string => (x ? x[locale] || x.en : '');

export interface ComponentChoice {
	on: boolean;
	version: string;
	features: string[];
	/** Text of every input; booleans for checkboxes. Only keys the user touched are kept. */
	settings: Record<string, string | boolean>;
}

export interface ComposerForm {
	name: string;
	distro: string;
	release: string;
	digest: string;
	platforms: string[];
	components: Record<string, ComponentChoice>;
	registry: string;
	repository: string;
	tag: string;
}

const versionCmp = (a: string, b: string) => {
	const x = a.split('.').map(Number);
	const y = b.split('.').map(Number);
	for (let i = 0; i < Math.max(x.length, y.length); i++) {
		const d = (y[i] ?? 0) - (x[i] ?? 0);
		if (d !== 0 && !Number.isNaN(d)) return d;
	}
	return 0;
};

/** Versions of a component that exist on the release, newest first. */
export const versionsOn = (comp: ComposerComponent, distro: string, release: string): string[] =>
	comp.versions
		.filter((v) => v.support.some((s) => s.distro === distro && s.release === release))
		.map((v) => v.version)
		.sort(versionCmp);

export const releasesOf = (cat: ComposerCatalog, distro: string) => cat.distros.find((d) => d.id === distro)?.releases ?? [];
export const familyOf = (cat: ComposerCatalog, distro: string) => cat.distros.find((d) => d.id === distro)?.family ?? '';

/** Features the distribution family offers for the component. */
export const featuresOn = (cat: ComposerCatalog, comp: ComposerComponent, distro: string): string[] => {
	const family = familyOf(cat, distro);
	return (comp.features ?? []).filter((f) => family in f.packages).map((f) => f.id);
};

/** Architectures every selected component offers on the release, as OCI platforms. */
export const platformsOn = (cat: ComposerCatalog, form: ComposerForm): string[] => {
	const rel = releasesOf(cat, form.distro).find((r) => r.id === form.release);
	if (!rel) return [];
	let archs = [...rel.architectures];
	for (const comp of cat.components) {
		const c = form.components[comp.id];
		if (!c?.on) continue;
		const s = comp.versions.find((v) => v.version === c.version)?.support.find((x) => x.distro === form.distro && x.release === form.release);
		if (s) archs = archs.filter((a) => s.architectures.includes(a));
	}
	return archs.map((a) => 'linux/' + a);
};

export const defaultText = (f: ComposerField): string => String(f.default);

/** Settings that apply for the current selection (their precondition holds). */
export const applicableFields = (comp: ComposerComponent, form: ComposerForm): ComposerField[] =>
	(comp.settings ?? []).filter(
		(f) => (!f.needs || form.components[f.needs]?.on) && (!f.needsFeature || form.components[comp.id]?.features.includes(f.needsFeature))
	);

export const currentValue = (f: ComposerField, c: ComponentChoice): string | boolean => (f.key in c.settings ? c.settings[f.key] : f.type === 'bool' ? (f.default as boolean) : defaultText(f));

const emptyChoice = (comp: ComposerComponent, version = ''): ComponentChoice => ({ on: false, version: version || comp.versions[0]?.version || '', features: [], settings: {} });

/** A reasonable first recipe: Alpine with nginx and PHP. */
export function defaultForm(cat: ComposerCatalog): ComposerForm {
	const form: ComposerForm = {
		name: '',
		distro: cat.distros[0]?.id ?? '',
		release: cat.distros[0]?.releases[0]?.id ?? '',
		digest: '',
		platforms: ['linux/amd64'],
		components: {},
		registry: '',
		repository: '',
		tag: ''
	};
	for (const comp of cat.components) form.components[comp.id] = emptyChoice(comp);
	for (const id of ['nginx', 'php']) {
		const c = form.components[id];
		if (c) c.on = true;
	}
	const php = form.components['php'];
	if (php) php.features = ['mbstring', 'mysqli', 'opcache'];
	return reconcile(cat, form);
}

/**
 * Brings a form in line with the catalog after the base or a version changed:
 * versions move to the newest one the release offers (or the component is
 * switched off when it has none), unavailable features and settings that no
 * longer apply are dropped, requirements are kept, and platforms are limited
 * to what every selected component offers.
 */
export function reconcile(cat: ComposerCatalog, form: ComposerForm): ComposerForm {
	const out: ComposerForm = { ...form, components: {} };
	if (!releasesOf(cat, out.distro).some((r) => r.id === out.release)) out.release = releasesOf(cat, out.distro)[0]?.id ?? '';
	for (const comp of cat.components) {
		const cur = form.components[comp.id] ?? emptyChoice(comp);
		const avail = versionsOn(comp, out.distro, out.release);
		const version = avail.includes(cur.version) ? cur.version : (avail[0] ?? cur.version);
		const okFeatures = featuresOn(cat, comp, out.distro);
		out.components[comp.id] = { ...cur, version, on: cur.on && avail.length > 0, features: cur.features.filter((f) => okFeatures.includes(f)), settings: { ...cur.settings } };
	}
	// Requirements: switching off what a selected component needs would be invalid.
	for (const comp of cat.components) {
		const c = out.components[comp.id];
		if (c.on && (comp.requires ?? []).some((r) => !out.components[r]?.on)) c.on = false;
	}
	for (const comp of cat.components) {
		const c = out.components[comp.id];
		const keep = new Set(applicableFields(comp, out).map((f) => f.key));
		c.settings = Object.fromEntries(Object.entries(c.settings).filter(([k]) => keep.has(k)));
	}
	const allowed = platformsOn(cat, out);
	out.platforms = out.platforms.filter((p) => allowed.includes(p));
	if (out.platforms.length === 0 && allowed.length > 0) out.platforms = [allowed[0]];
	return out;
}

const intRE = /^-?\d+$/;

/** Typed value of a setting input; text that is not a number stays text so the manager reports it. */
export const typedValue = (f: ComposerField, v: string | boolean): string | boolean | number => (f.type === 'int' && typeof v === 'string' && intRE.test(v.trim()) ? Number(v) : typeof v === 'string' ? v.trim() : v);

/** The recipe the manager validates: only values that differ from the catalog default are sent. */
export function buildRecipe(cat: ComposerCatalog, form: ComposerForm): ComposerRecipe {
	const components: ComposerRecipe['components'] = [];
	for (const comp of cat.components) {
		const c = form.components[comp.id];
		if (!c?.on) continue;
		const settings: Record<string, unknown> = {};
		for (const f of applicableFields(comp, form)) {
			if (!(f.key in c.settings)) continue;
			const v = c.settings[f.key];
			const same = f.type === 'bool' ? v === f.default : String(v).trim() === defaultText(f);
			if (!same) settings[f.key] = typedValue(f, v);
		}
		components.push({ id: comp.id, version: c.version, ...(c.features.length ? { features: [...c.features].sort() } : {}), ...(Object.keys(settings).length ? { settings } : {}) });
	}
	return { name: form.name.trim(), base: { distro: form.distro, release: form.release, digest: form.digest.trim() }, platforms: [...form.platforms].sort(), components };
}

/** Findings addressed to exactly this path or below it. */
export const issuesAt = (issues: ComposerIssue[], path: string): ComposerIssue[] => issues.filter((i) => i.path === path || i.path.startsWith(path + '.'));

export const settingPath = (componentId: string, key: string) => `components[${componentId}].settings.${key}`;

/** Dependencies shown next to a component: which selected components need it. */
export const requiredBy = (cat: ComposerCatalog, form: ComposerForm, id: string): string[] =>
	cat.components.filter((c) => form.components[c.id]?.on && (c.requires ?? []).includes(id)).map((c) => c.id);
