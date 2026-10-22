// Light/dark mode and accent color, stored per browser. app.html applies the
// stored values before first paint with the same storage keys.

export const themeModes = ['system', 'light', 'dark'] as const;
export type ThemeMode = (typeof themeModes)[number];
export const accents = ['zinc', 'blue', 'green', 'violet', 'orange'] as const;
export type Accent = (typeof accents)[number];

const MODE_KEY = 'theme';
const ACCENT_KEY = 'accent';

function read<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
	try {
		const v = localStorage.getItem(key);
		if (v && (allowed as readonly string[]).includes(v)) return v as T;
	} catch {
		// storage unavailable
	}
	return fallback;
}

function write(key: string, value: string) {
	try {
		localStorage.setItem(key, value);
	} catch {
		// not persisted; applies to this page view only
	}
}

let mode = $state<ThemeMode>(read(MODE_KEY, themeModes, 'system'));
let accent = $state<Accent>(read(ACCENT_KEY, accents, 'zinc'));

const media = typeof matchMedia === 'function' ? matchMedia('(prefers-color-scheme: dark)') : undefined;

function apply() {
	const dark = mode === 'dark' || (mode === 'system' && !!media?.matches);
	document.documentElement.classList.toggle('dark', dark);
	document.documentElement.style.colorScheme = dark ? 'dark' : 'light';
	document.documentElement.dataset.accent = accent;
}

media?.addEventListener('change', () => mode === 'system' && apply());

export const theme = {
	get mode() {
		return mode;
	},
	get accent() {
		return accent;
	},
	setMode(next: ThemeMode) {
		mode = next;
		write(MODE_KEY, next);
		apply();
	},
	setAccent(next: Accent) {
		accent = next;
		write(ACCENT_KEY, next);
		apply();
	},
	apply
};
