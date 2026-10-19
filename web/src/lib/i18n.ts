export const locales = ['en', 'de'] as const;
export type Locale = (typeof locales)[number];

const messages = {
	en: {
		appName: 'DB Cluster Manager',
		prototype: 'Prototype',
		title: 'Manager',
		intro:
			'Prototype. No Swarm is connected and no cluster operations exist yet; only the manager process is reported.',
		processStatus: 'Manager process',
		apiHealth: 'API health',
		healthOk: 'Serving requests',
		checking: 'Checking…',
		unavailable: 'Unavailable',
		version: 'Version',
		commit: 'Commit',
		go: 'Go',
		platform: 'Platform',
		language: 'Language'
	},
	de: {
		appName: 'DB Cluster Manager',
		prototype: 'Prototyp',
		title: 'Manager',
		intro:
			'Prototyp. Es ist kein Swarm verbunden und es gibt noch keine Cluster-Operationen; angezeigt wird nur der Manager-Prozess.',
		processStatus: 'Manager-Prozess',
		apiHealth: 'API-Status',
		healthOk: 'Nimmt Anfragen an',
		checking: 'Wird geprüft …',
		unavailable: 'Nicht verfügbar',
		version: 'Version',
		commit: 'Commit',
		go: 'Go',
		platform: 'Plattform',
		language: 'Sprache'
	}
} as const;

export type MessageKey = keyof (typeof messages)['en'];

export function isLocale(value: unknown): value is Locale {
	return typeof value === 'string' && (locales as readonly string[]).includes(value);
}

/** Picks the first supported language from a browser preference list; defaults to English. */
export function detectLocale(preferred: readonly string[]): Locale {
	for (const tag of preferred) {
		const base = tag.toLowerCase().split('-')[0];
		if (isLocale(base)) return base;
	}
	return 'en';
}

/** Keys defined for a locale; used by tests to keep all locales complete. */
export function messageKeys(locale: Locale): string[] {
	return Object.keys(messages[locale]).sort();
}

export function translate(locale: Locale, key: MessageKey): string {
	return messages[locale][key];
}
