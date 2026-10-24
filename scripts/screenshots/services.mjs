// Mock-server captures for the service UI. No production or Swarm access.
// Copy this script beside a scratch installation of Playwright outside the repo.
// Start: cd web && npm run dev:mock -- --host 127.0.0.1 --port 5199 --strictPort
// Run: node services.mjs <output-directory>; optional BASE=http://127.0.0.1:5199
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';
import { strict as assert } from 'node:assert';
const out = process.argv[2];
if (!out) throw new Error('Provide an output directory');
mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
try {
 for (const locale of ['en', 'de']) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1600 }, locale, reducedMotion: 'reduce' });
  await context.addInitScript(l => { localStorage.setItem('locale', l); localStorage.setItem('theme', 'light'); }, locale);
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(String(e)));
  const settle = async () => { await page.waitForLoadState('networkidle'); };
  const capture = async name => {
   assert.equal(errors.length, 0, errors.join('\n'));
   assert.equal(await page.getByRole('note').filter({ hasText: 'MOCK DATA' }).count(), 1);
   await page.screenshot({ path: `${out}/${locale}-light-services-${name}-mock.png`, fullPage: true });
  };
  await page.goto((process.env.BASE ?? 'http://127.0.0.1:5199') + '/services');
  await settle();
  const re = (en, de) => new RegExp(locale === 'de' ? de : en);
  await page.locator('tr', { hasText: 'shop-web' }).getByRole('button', { name: re('Update service', 'Service aktualisieren') }).click();
  const update = page.getByRole('region', { name: re('Update service', 'Service aktualisieren') });
  await update.getByLabel(re('^Image$', '^Image$')).fill('nginx:1.28');
  await update.getByRole('button', { name: re('Show plan', 'Plan anzeigen') }).click();
  await settle();
  await update.locator('summary').filter({ hasText: re('Expert view', 'Expertenansicht') }).first().click();
  await update.locator('summary').filter({ hasText: re('Prepared rollback plan', 'Vorbereiteter Rollback-Plan') }).click();
  await capture('update');
  const downloadEvent = page.waitForEvent('download');
  await update.getByRole('button', { name: re('Download JSON', 'JSON herunterladen') }).first().click();
  assert.match((await downloadEvent).suggestedFilename(), /\.json$/);
  await page.reload(); await settle();
  await page.locator('#sv-type').selectOption('s3');
  await page.locator('#sv-swarm').selectOption('mock-swarm-a');
  await page.locator('#sv-name').fill('mock-object-store');
  await page.locator('#sv-image').fill('example.invalid/object-store:1');
  await page.locator('#sv-host').selectOption('mock-host-2');
  await page.locator('#detail-rootUserSecret').fill('mock-root-user');
  await page.locator('#detail-rootPasswordSecret').fill('mock-root-password');
  await page.getByRole('button', { name: re('Show plan', 'Plan anzeigen') }).click(); await settle();
  await capture('s3');
  await page.locator('#sv-type').selectOption('nfs');
  await page.locator('#sv-image').fill('example.invalid/nfs:1');
  await page.locator('#sv-host').selectOption('mock-host-2');
  await page.getByLabel(re('Allowed IPs', 'Erlaubte IPs')).fill('192.0.2.0/24');
  await page.getByRole('button', { name: re('Show plan', 'Plan anzeigen') }).click(); await settle();
  assert.equal(await page.getByRole('button', { name: re('Request with this plan', 'Mit diesem Plan anfordern') }).isDisabled(), true);
  await capture('nfs');
  await context.close();
 }
} finally { await browser.close(); }
