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
	// Services: container request with plan and approval; Traefik plan with blockers.
	const de = run.locale === 'de';
	const re = (en, d) => new RegExp(de ? d : en, 'i');
	const card = (en, d) => page.locator('[data-slot="card"]', { has: page.getByRole('heading', { level: 2, name: re(en, d) }) }).first();
	await page.setViewportSize({ width: 1440, height: 3200 });
	await page.goto(BASE + '/services');
	await settle();
	await page.locator('#sv-swarm').selectOption('mock-swarm-a');
	await page.locator('#sv-name').fill('shop-api');
	await page.locator('#sv-image').fill('registry.example.invalid/team/shop-api:2.3.1');
	await page.locator('#sv-env').fill('TZ=Europe/Berlin\nDB_PASSWORD_FILE=/run/secrets/db_password');
	await page.locator('#sv-sec').fill('db_password');
	await page.locator('#sv-mem').fill('512');
	await page.getByRole('button', { name: re('Show plan', 'Plan anzeigen') }).click();
	await settle();
	await page.getByRole('button', { name: re('Request with this plan', 'Mit diesem Plan anfordern') }).click();
	await settle();
	await page.locator('#main').screenshot({ path: name('services') });
	console.log('shot', name('services'));
	await page.goto(BASE + '/services');
	await settle();
	await page.locator('#sv-swarm').selectOption('mock-swarm-a');
	await page.locator('#sv-type').selectOption('traefik');
	await page.locator('#sv-name').fill('edge');
	await page.locator('#sv-host').selectOption('mock-host-1');
	await page.locator('#sv-mail').fill('ops@example.org');
	await page.locator('#sv-rh-0').fill('shop.example.org');
	await page.locator('#sv-rb-0').fill('http://shop-web:8080');
	await page.getByRole('button', { name: re('Show plan', 'Plan anzeigen') }).click();
	await settle();
	await card('Request a service', 'Dienst anfordern').screenshot({ path: name('services-traefik-plan') });
	console.log('shot', name('services-traefik-plan'));
	// Image composer with the rendered Dockerfile and plan.
	await page.goto(BASE + '/images');
	await settle();
	await page.locator('#im-name').fill('Shop web');
	await page.locator('#im-digest').fill('sha256:' + 'a1'.repeat(32));
	await page.locator('#im-reg').selectOption('ghcr-main');
	await page.locator('#im-repo').fill('team/shop-web');
	await page.locator('#im-tag').fill('1.1.0');
	await page.locator('#im-snip').fill('location /health {\n    return 200 "ok";\n}');
	await page.getByRole('button', { name: re('Show Dockerfile and plan', 'Dockerfile und Plan anzeigen') }).click();
	await settle();
	await page.locator('#main').screenshot({ path: name('images') });
	console.log('shot', name('images'));
	// Registries: list and the add form with a typed (masked) password.
	await page.goto(BASE + '/registries');
	await settle();
	await page.locator('#rg-id').fill('dockerhub-team');
	await page.locator('#rg-name').fill('Docker Hub (team)');
	await page.locator('#rg-kind').selectOption('dockerhub');
	await page.locator('#rg-user').fill('team-ci');
	await page.locator('#rg-pw').fill('not-a-real-token');
	await page.screenshot({ path: name('registries') });
	console.log('shot', name('registries'));
	// Host maintenance plan: a refused host and a safe host.
	await page.goto(BASE + '/hosts');
	await settle();
	await page.locator('tr', { hasText: 'mock-host-2' }).getByRole('button', { name: re('Maintenance', 'Wartung') }).click();
	await page.getByRole('button', { name: re('Show maintenance plan', 'Wartungsplan anzeigen') }).click();
	await settle();
	await page.locator('#main').screenshot({ path: name('host-maintenance') });
	console.log('shot', name('host-maintenance'));
	// Drift check card of the cluster detail.
	await page.goto(BASE + '/clusters/mock-galera');
	await settle();
	await card('Drift check', 'Abweichungsprüfung').screenshot({ path: name('drift') });
	console.log('shot', name('drift'));
	// Backup policy form with the logical method and retention.
	await page.goto(BASE + '/backups');
	await settle();
	await page.getByRole('button', { name: re('Edit schedule', 'Zeitplan bearbeiten') }).first().click();
	await page.locator('select[id^="bp-method-"]').first().selectOption('logical');
	await page.locator('input[id^="bp-dbs-"]').first().fill('shop, customers');
	await settle();
	await card('Manage', 'Verwalten').screenshot({ path: name('backup-policy') });
	console.log('shot', name('backup-policy'));
	// Replica keyring settings.
	await page.setViewportSize({ width: 1440, height: 1500 });
	await page.goto(BASE + '/settings/replicas');
	await settle();
	await page.locator('#main').screenshot({ path: name('keyring') });
	console.log('shot', name('keyring'));
	await ctx.close();
}
await browser.close();
