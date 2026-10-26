import { describe, expect, it } from 'vitest';
import { detectLocale, locales, messageKeys, placeholders, translate } from './i18n';
import { audit, catalogIssues, svelteIssues } from '../../scripts/check_i18n.mjs';

describe('i18n', () => {
	it('detects the first supported language and defaults to English', () => {
		expect(detectLocale(['fr-FR', 'de-AT', 'en'])).toBe('de');
		expect(detectLocale(['fr'])).toBe('en');
		expect(detectLocale([])).toBe('en');
	});
	it('defines the same keys for every locale with non-empty text', () => {
		const reference = messageKeys('en');
		for (const l of locales) {
			expect(messageKeys(l)).toEqual(reference);
			for (const k of reference) expect(translate(l, k as never)).not.toBe('');
		}
	});
	it('uses the same placeholders in every locale', () => {
		for (const k of messageKeys('en')) {
			const ref = placeholders('en', k as never);
			for (const l of locales) expect(placeholders(l, k as never), `${l}.${k}`).toEqual(ref);
		}
	});
	it('substitutes parameters and leaves unknown placeholders visible', () => {
		expect(translate('en', 'signedInAs', { account: 'admin' })).toBe('Signed in as admin');
		expect(translate('de', 'nodeCount', { count: 3 })).toBe('3 Knoten');
		expect(translate('en', 'signedInAs')).toBe('Signed in as {account}');
	});
});

describe('static UI translations', () => {
	it('keeps both catalogs and Svelte text complete', () => {
		expect(audit()).toEqual([]);
	});
	it('reports missing keys, placeholder drift and untranslated visible text', () => {
		expect(catalogIssues("const en = { save: 'Save {name}' } as const; const de = { save: 'Speichern {id}', extra: 'Mehr' };"))
			.toEqual(['Missing en.extra', 'Placeholder mismatch save']);
		expect(svelteIssues('<button aria-label="Save">Save</button>')).toHaveLength(2);
		expect(svelteIssues('<input placeholder={ready ? "Save" : "Wait"} />')).toHaveLength(2);
	});
});
