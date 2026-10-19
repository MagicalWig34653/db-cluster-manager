import { describe, expect, it } from 'vitest';
import { detectLocale, locales, messageKeys, translate } from './i18n';

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
});
