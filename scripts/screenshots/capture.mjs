// Capture the documentation screenshots from the development mock server.
//
// Playwright is deliberately not a dependency of this repository. Install it
// in a scratch directory outside the repository and copy this file there,
// because ES module imports resolve next to the script:
//
//   (cd web && npm run dev:mock -- --port 5199 --strictPort) &
//   mkdir /tmp/shots && cp scripts/screenshots/capture.mjs /tmp/shots/
//   cd /tmp/shots && npm init -y && npm i playwright && npx playwright install chromium
//   node capture.mjs /tmp/shots/raw
//   <python with Pillow> scripts/screenshots/optimize.py /tmp/shots/raw docs/assets/screenshots
//
// Every capture shows invented mock data and the "MOCK DATA" banner; it must
// be captioned as a mock-server screenshot wherever it is used.
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const BASE = process.env.BASE ?? 'http://localhost:5199';
const OUT = process.argv[2];
mkdirSync(OUT, { recursive: true });

const views = [
	['overview', '/'],
	['clusters', '/clusters'],
	['cluster-detail', '/clusters/mock-galera'],
	['create-wizard', '/clusters/new'],
	['backups', '/backups'],
	['operations', '/operations'],
	['notification-settings', '/notifications/settings'],
	['hosts', '/hosts']
];

const runs = [
	{ locale: 'en', theme: 'light', only: null },
	{ locale: 'de', theme: 'light', only: null },
	{ locale: 'en', theme: 'dark', only: ['overview', 'cluster-detail', 'operations'] }
];

const browser = await chromium.launch();
for (const run of runs) {
	const ctx = await browser.newContext({
		viewport: { width: 1440, height: 900 },
		deviceScaleFactor: 1,
		colorScheme: run.theme,
		reducedMotion: 'reduce',
		locale: run.locale === 'de' ? 'de-DE' : 'en-US'
	});
	await ctx.addInitScript(([l, t]) => {
		localStorage.setItem('locale', l);
		localStorage.setItem('theme', t);
	}, [run.locale, run.theme]);
	const page = await ctx.newPage();
	const settle = async () => {
		await page.waitForLoadState('networkidle');
		await page.waitForTimeout(800);
	};
	const name = (v) => `${OUT}/${run.locale}-${run.theme}-${v}.png`;
	for (const [v, path] of views) {
		if (run.only && !run.only.includes(v)) continue;
		await page.goto(BASE + path);
		await settle();
		await page.screenshot({ path: name(v) });
		console.log('shot', name(v));
	}
	if (run.only) {
		await ctx.close();
		continue;
	}
	// Outage recovery: diagnosis, then plan with approval and progress. A tall
	// viewport keeps the sticky top bar from overlapping element captures.
	await page.setViewportSize({ width: 1440, height: 3200 });
	await page.goto(BASE + '/clusters/mock-galera');
	await settle();
	await page.getByRole('button', { name: /Run diagnosis|Diagnose starten/ }).click();
	await settle();
	await page.getByRole('button', { name: /Plan recovery with|Wiederherstellung mit/ }).click();
	await settle();
	const outage = page.locator('[data-slot="card"]', { has: page.getByRole('heading', { level: 2, name: /outage|Ausfall/i }) }).first();
	await outage.screenshot({ path: name('outage-recovery') });
	console.log('shot', name('outage-recovery'));
	// Delete: typed confirmation, then the stored plan waiting for approval.
	await page.locator('#del-name').fill('Mock Galera');
	await page.getByRole('button', { name: /Plan deletion|Löschen planen/ }).click();
	await settle();
	const del = page.locator('[data-slot="card"]', { has: page.getByRole('heading', { level: 2, name: /Delete cluster|Cluster löschen/ }) }).first();
	await del.screenshot({ path: name('delete-approval') });
	console.log('shot', name('delete-approval'));
	await ctx.close();
}
await browser.close();
