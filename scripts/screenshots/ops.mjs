// Mock-server captures for the Galera operation planner, proxy writer switch,
// data-key rotation and metastore membership panels. No production access.
// Copy beside a scratch Playwright installation outside the repo (see capture.mjs).
// Start: cd web && npm run dev:mock -- --host 127.0.0.1 --port 5199 --strictPort
// Run: node ops.mjs <output-directory>
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const BASE = process.env.BASE ?? 'http://127.0.0.1:5199';
const OUT = process.argv[2];
if (!OUT) throw new Error('Provide an output directory');
mkdirSync(OUT, { recursive: true });
const browser = await chromium.launch();
try {
	for (const locale of ['en', 'de']) {
		const ctx = await browser.newContext({ viewport: { width: 1440, height: 3600 }, locale: locale === 'de' ? 'de-DE' : 'en-US', reducedMotion: 'reduce' });
		await ctx.addInitScript((l) => {
			localStorage.setItem('locale', l);
			localStorage.setItem('theme', 'light');
		}, locale);
		const page = await ctx.newPage();
		const errors = [];
		page.on('pageerror', (e) => errors.push(String(e)));
		const settle = async () => {
			await page.waitForLoadState('networkidle');
			await page.waitForTimeout(600);
		};
		const card = (name) => page.locator('[data-slot="card"]', { has: page.getByRole('heading', { level: 2, name }) }).first();
		const re = (en, de) => new RegExp(locale === 'de' ? de : en);
		const shot = async (loc, name) => {
			await loc.screenshot({ path: `${OUT}/${locale}-light-${name}.png` });
			console.log('shot', locale, name);
		};

		await page.goto(BASE + '/clusters/mock-galera');
		await settle();
		const plan = card(re('Galera operations', 'Galera-Operationen'));
		await plan.locator('#cp-kind').selectOption('minor-upgrade');
		await plan.locator('#cp-image').fill('mariadb:11.4.3@sha256:' + 'a'.repeat(64));
		await plan.getByRole('button', { name: re('Create plan', 'Plan erstellen') }).click();
		await settle();
		await shot(plan, 'cluster-operation-plan');

		const pw = card(re('Proxy writer switch', 'Proxy-Writer wechseln'));
		await pw.locator('#pw-writer').selectOption({ index: 2 });
		await pw.locator('#pw-token').fill('mock-token');
		await pw.getByRole('checkbox').check();
		await pw.getByRole('button', { name: re('Request switch', 'Wechsel anfordern') }).click();
		await settle();
		await shot(pw, 'proxy-writer');

		await page.goto(BASE + '/settings/replicas');
		await settle();
		const rot = card(re('Data-key rotation', 'Datenschlüssel-Rotation'));
		await rot.getByRole('button', { name: re('Preview rotation', 'Rotation ansehen') }).click();
		await settle();
		await rot.getByRole('checkbox').check();
		await shot(rot, 'key-rotation');

		const mem = card(re('Metastore membership', 'Metastore-Mitgliedschaft'));
		await mem.locator('#mem-replica').selectOption('manager-3');
		await mem.getByRole('button', { name: re('Preview removal', 'Entfernen ansehen') }).click();
		await settle();
		await mem.getByRole('checkbox').check();
		await shot(mem, 'metastore-membership');
		if (errors.length) throw new Error(errors.join('\n'));
		await ctx.close();
	}
} finally {
	await browser.close();
}
