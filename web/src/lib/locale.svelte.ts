import { detectLocale, isLocale, translate, type Locale, type MessageKey, type Params } from './i18n';

const STORAGE_KEY = 'locale';

function initial(): Locale {
	try {
		const stored = localStorage.getItem(STORAGE_KEY);
		if (isLocale(stored)) return stored;
	} catch {
		// storage unavailable: fall through to browser preference
	}
	return detectLocale(typeof navigator === 'undefined' ? [] : navigator.languages);
}

let current = $state<Locale>(initial());

export const locale = {
	get value() {
		return current;
	},
	set(next: Locale) {
		current = next;
		try {
			localStorage.setItem(STORAGE_KEY, next);
		} catch {
			// not persisted; the choice still applies for this page view
		}
		document.documentElement.lang = next;
	}
};

export const t = (key: MessageKey, params?: Params) => translate(current, key, params);
