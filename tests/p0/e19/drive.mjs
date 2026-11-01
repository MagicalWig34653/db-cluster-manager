// SPDX-License-Identifier: AGPL-3.0-only
//
// E19 driver (host side): drives the real manager of tests/p0/e19-copy-archive.sh
// through its HTTP API and the real web UI (Chromium with a virtual WebAuthn
// authenticator). Playwright is deliberately not a repository dependency:
// copy this file into a scratch directory with `npm i playwright` and run
//
//   node drive.mjs <command> [args...]
//
// Commands run one after the other on the kept lab VMs; the state (virtual
// passkey, ids, timestamps) is kept in $E19_STATE (default ./state.json).
// Secrets are never printed: SQL runs inside the VMs, passwords stay there.
import { chromium } from 'playwright';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync, appendFileSync } from 'node:fs';

const BASE = process.env.E19_BASE ?? 'http://localhost:18480';
const STATE_FILE = process.env.E19_STATE ?? './state.json';
const LOGFILE = process.env.E19_DRIVE_LOG ?? './drive.log';
const SHOTS = process.env.E19_SHOTS ?? './shots';
const M = ['dbcm-lab-m1', 'dbcm-lab-m2', 'dbcm-lab-m3'];
const INST = 'e19lab';
const CLUSTER = 'e19db';
const MARIADB = 'mariadb:11.4.13@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873';

const st = existsSync(STATE_FILE) ? JSON.parse(readFileSync(STATE_FILE, 'utf8')) : {};
const save = () => writeFileSync(STATE_FILE, JSON.stringify(st, null, 1));
const now = () => new Date().toISOString().slice(11, 23);
const log = (...a) => {
	const l = `${now()} ${a.join(' ')}`;
	console.log(l);
	appendFileSync(LOGFILE, l + '\n');
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function vm(name, script, input) {
	return execFileSync('limactl', ['shell', '--workdir', '/', name, '--', 'sudo', 'sh', '-c', script], {
		input, encoding: 'utf8', maxBuffer: 64 << 20
	});
}

// ------------------------------------------------------------ browser/session
let browser, ctx, page, cdp, authId;
async function open() {
	browser = await chromium.launch({ headless: process.env.E19_HEADED !== '1' });
	ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, baseURL: BASE, locale: 'en-US' });
	await ctx.addInitScript(() => { try { localStorage.setItem('locale', 'en'); } catch { /* ignore */ } });
	page = await ctx.newPage();
	cdp = await ctx.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const r = await cdp.send('WebAuthn.addVirtualAuthenticator', {
		options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true }
	});
	authId = r.authenticatorId;
	for (const c of st.credentials ?? []) await cdp.send('WebAuthn.addCredential', { authenticatorId: authId, credential: { ...c, signCount: Math.floor(Date.now() / 1000) } }); // monotonic across runs: the manager refuses a counter that does not grow
}
async function close() {
	if (cdp && authId) {
		const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId: authId });
		if (credentials.length) st.credentials = credentials;
		save();
	}
	await browser?.close();
}
async function shot(name) {
	await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: true }).catch(() => {});
}
async function login() {
	await page.goto('/login');
	await page.waitForLoadState('networkidle');
	const btn = page.getByRole('button', { name: /passkey/i }).first();
	await btn.click();
	await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20000 });
	log('login ok at', page.url());
}
// api(method, path, body) -> { status, body } through the browser session.
async function api(method, path, body) {
	const res = await ctx.request.fetch(`${BASE}/api/v1${path}`, {
		method,
		headers: { Origin: BASE, ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
		data: body !== undefined ? JSON.stringify(body) : undefined,
		failOnStatusCode: false
	});
	const text = await res.text();
	let json;
	try { json = JSON.parse(text); } catch { json = text; }
	return { status: res.status(), body: json, headers: res.headers() };
}
const redact = (o) => JSON.stringify(o, (k, v) => (/password|secret/i.test(k) && typeof v === 'string' && k !== 'rootPasswordSecret' ? '<redacted>' : v));
async function must(method, path, body, ok = [200, 201, 202, 204]) {
	const r = await api(method, path, body);
	log(`${method} ${path} -> ${r.status} ${redact(r.body).slice(0, 400)}`);
	if (!ok.includes(r.status)) throw new Error(`${method} ${path}: HTTP ${r.status}`);
	return r.body;
}
async function session() {
	await open();
	await page.goto('/');
	const r = await api('GET', '/auth/session');
	if (r.status !== 200 || !(r.body?.authenticated ?? r.body?.account)) await login();
}
async function waitOp(id, { until = ['succeeded', 'failed', 'aborted'], timeout = 1500, label = '' } = {}) {
	const t0 = Date.now();
	let last = '';
	while ((Date.now() - t0) / 1000 < timeout) {
		const ops = (await api('GET', '/operations')).body.items ?? [];
		const op = ops.find((o) => o.id === id);
		const s = op ? `${op.state} ${op.reason ?? ''}`.trim() : 'unknown';
		if (s !== last) { log(`op ${id} ${label}: ${s}`); last = s; }
		if (op && until.includes(op.state)) return op;
		await sleep(5000);
	}
	throw new Error(`operation ${id} not finished after ${timeout}s: ${last}`);
}

// ------------------------------------------------------------ SQL in the VMs
function nodeVm(n) {
	const host = vm(M[0], `docker service ps --filter desired-state=running --format '{{.Node}}' dbcm-${CLUSTER}-n${n} | head -1`).trim();
	return host.replace(/^lima-/, '');
}
function rsql(n, sql) {
	const v = nodeVm(n);
	return vm(v, `c=$(docker ps -q --no-trunc -f label=com.docker.swarm.service.name=dbcm-${CLUSTER}-n${n} -f status=running | head -1); test -n "$c" || exit 7; docker exec -i "$c" sh -c 'exec mariadb -S /dbcm/run/mysqld.sock -uroot -p"$(cat /run/secrets/dbcm-e4-root-pw)" -N -B'`, sql + '\n').trim();
}

const cmds = {};

// ------------------------------------------------------------ administrator
cmds.setup = async () => {
	const token = vm(M[0], `journalctl -u dbcm-e4-manager --no-pager | grep -o 'setup token[^:]*: [^ ]*' | tail -1 | awk '{print $NF}'`).trim();
	await open();
	await page.goto('/setup');
	await page.fill('#setup-token', token);
	await page.fill('#account-id', 'admin');
	await page.fill('#display-name', 'E19 lab admin');
	await page.getByRole('button', { name: /Register passkey/i }).click();
	await page.waitForSelector('#codes-title', { timeout: 30000 });
	await shot('setup-recovery-codes');
	await page.getByLabel(/stored these recovery codes/i).check();
	await page.getByRole('button', { name: /Continue to the manager/i }).click();
	await page.waitForLoadState('networkidle');
	log('administrator created, url', page.url());
	log('session', redact((await api('GET', '/auth/session')).body));
	await close();
};

// ------------------------------------------------------------ swarm and hosts
cmds.swarm = async () => {
	await session();
	let sw = ((await api('GET', '/swarms')).body.items ?? [])[0];
	if (!sw) sw = await must('POST', '/swarms', { name: 'lab', dockerHost: 'unix:///var/run/docker.sock' });
	st.swarm = sw.id; save();
	await must('POST', `/swarms/${sw.id}/sync`);
	for (const h of (await api('GET', '/hosts')).body.items) await must('POST', `/hosts/${h.nodeId}/enroll`, {});
	await must('POST', `/swarms/${sw.id}/agents/deploy`);
	for (let i = 0; i < 60; i++) {
		const n = ((await api('GET', '/hosts')).body.items ?? []).filter((h) => h.agentIdentity).length;
		log('agents enrolled:', n);
		if (n === 3) break;
		await sleep(5000);
	}
	await close();
};

// ------------------------------------------------------------ cluster with binlog
cmds.cluster = async () => {
	await session();
	const body = {
		swarm: st.swarm, name: CLUSTER, size: 3, image: MARIADB, network: 'dbcm-e19-net', rootPasswordSecret: 'dbcm-e4-root-pw',
		gcacheSize: '128M', binlog: true,
		resources: { limits: { milli_cpu: 1000, memory_bytes: 1073741824 }, reservations: { milli_cpu: 250, memory_bytes: 536870912 } },
		placement: { reserve: {} }
	};
	const r = await must('POST', '/clusters', body);
	const op = await waitOp(r.id ?? r.operation?.id ?? r.operationId, { label: 'create cluster' });
	if (op.state !== 'succeeded') throw new Error('cluster creation failed');
	const cl = (await api('GET', '/clusters')).body.items.find((c) => c.name === CLUSTER);
	st.cluster = cl.id; save();
	log('cluster', cl.id);
	log('log_bin / binlog_format / cluster size:', rsql(1, "SELECT @@log_bin, @@binlog_format, VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_cluster_size'").replace(/\t/g, ' '));
	await close();
};


// ------------------------------------------------------------ part 1: backup, copy, restore of the copy
const CHECK = "SELECT COUNT(*), SUM(amount), SUM(id) FROM e19.orders; SELECT COUNT(*) FROM e19.items; CHECKSUM TABLE e19.orders, e19.items";
const hostName = async (nodeId) => ((await api('GET', '/hosts')).body.items.find((h) => h.nodeId === nodeId) ?? {}).hostname;

cmds.data = async () => {
	await session();
	rsql(1, `CREATE DATABASE IF NOT EXISTS e19;
USE e19;
CREATE TABLE IF NOT EXISTS e19.orders (id INT PRIMARY KEY, batch VARCHAR(8) NOT NULL, amount INT NOT NULL, note VARCHAR(40)) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS e19.items (id INT PRIMARY KEY, name VARCHAR(40)) ENGINE=InnoDB;
INSERT INTO e19.orders SELECT seq, 'O', seq % 997, CONCAT('order-', seq) FROM seq_1_to_30000;
INSERT INTO e19.items SELECT seq, CONCAT('item-', seq) FROM seq_1_to_500;`);
	st.srcCheck = rsql(1, 'SET SESSION wsrep_sync_wait=15; ' + CHECK).replace(/\t/g, ' ');
	save();
	log('source state:', st.srcCheck.replace(/\n/g, ' | '));
	await close();
};

cmds.backup = async () => {
	await session();
	const r = await must('POST', `/clusters/${st.cluster}/backups`);
	const op = await waitOp(r.id, { label: 'backup', timeout: 900 });
	if (op.state !== 'succeeded') throw new Error('backup failed');
	const l = (await api('GET', '/backups')).body;
	const sum = (l.items ?? l).find((b) => b.cluster === st.cluster);
	log('backup summary:', redact({ lastBackupId: sum.lastBackupId, lastBackupAt: sum.lastBackupAt }));
	st.backup = sum.lastBackupId;
	save();
	log('backup', st.backup);
	await close();
};

async function copyBackup(backup, target = 'copy2') {
	const c = await must('POST', `/clusters/${st.cluster}/backup-copies`, { backup, target });
	for (let i = 0; i < 120; i++) {
		const cp = (await api('GET', '/backup-copies')).body.items.find((x) => x.id === c.id);
		if (cp && cp.status !== 'running') { log('copy', c.id, cp.status, cp.error ?? '', redact(cp.objects)); return cp; }
		await sleep(3000);
	}
	throw new Error('copy not finished');
}
cmds.copy = async () => {
	await session();
	const cp = await copyBackup(st.backup);
	if (cp.status !== 'succeeded') throw new Error('copy failed');
	st.copy = cp.id; save();
	await close();
};

// Without settings.backup.targetSecrets the restore of a copy must answer 503
// settings_incomplete and must NOT fall back to the main backend secret.
cmds.nosecret = async () => {
	await session();
	const r = await api('POST', '/restores/inspect', { cluster: st.cluster, copy: st.copy, host: (await api('GET', '/hosts')).body.items[0].nodeId, ttlMinutes: 30 });
	log('inspect copy without targetSecrets ->', r.status, redact(r.body));
	const ops = (await api('GET', '/operations')).body.items.filter((o) => /inspect|restore/.test(o.kind));
	log('restore operations left behind:', ops.length);
	await close();
};

async function inspectSql(started, sql) {
	const hn = await hostName(started.host);
	const v = 'dbcm-lab-' + hn.replace(/^lima-dbcm-lab-/, '');
	return vm(v, `c=$(docker ps -q --no-trunc -f label=com.docker.swarm.service.name=${started.service} -f status=running | head -1); test -n "$c" || exit 7; docker exec -i "$c" sh -c 'read -r pw; exec mariadb -h127.0.0.1 -P${started.port} -udbcm_inspect -p"$pw" -N -B'`, started.inspectionPassword + '\n' + sql + '\n').trim().replace(/\t/g, ' ');
}

// The restore of the copy through the real UI (source choice "Copy on a second target").
cmds.uirestore = async () => {
	await session();
	const hosts = (await api('GET', '/hosts')).body.items;
	const target = hosts.find((h) => /m2$/.test(h.hostname));
	await page.goto('/backups');
	await page.waitForLoadState('networkidle');
	await page.selectOption('#rs-cluster', st.cluster);
	await page.selectOption('#rs-host', target.nodeId);
	await page.getByLabel('Copy on a second target').check();
	await shot('restore-wizard-copy-choice');
	await page.selectOption('#rs-copy', st.copy);
	const opts = await page.locator('#rs-copy option').allTextContents();
	log('copy options in the UI:', JSON.stringify(opts));
	await page.getByRole('button', { name: 'Continue' }).click();
	await page.getByLabel(/I understand that the inspection instance/).check();
	let startedBody = null;
	page.on('response', async (res) => {
		if (res.url().endsWith('/api/v1/restores/inspect') && res.request().method() === 'POST') {
			log('UI POST /restores/inspect ->', res.status(), 'request', res.request().postData());
			try { startedBody = await res.json(); } catch { /* ignore */ }
		}
	});
	await page.getByRole('button', { name: 'Run inspection' }).click();
	await page.waitForSelector('text=I have saved the password', { timeout: 60000 }).catch(() => {});
	await sleep(1500);
	await shot('restore-copy-started');
	if (!startedBody?.id) throw new Error('inspection did not start; see shot');
	st.restoreOp = startedBody.id;
	st.restoreStarted = startedBody; // contains the one-time password: state file stays in the scratch dir
	save();
	log('started', startedBody.id, 'backup', startedBody.backup, 'service', startedBody.service, 'port', startedBody.port, 'host', await hostName(startedBody.host));
	await page.getByRole('button', { name: 'I have saved the password' }).click().catch(() => {});
	// approval through the operation card of the UI
	const confirm = page.getByLabel(/I have reviewed this plan/);
	await confirm.waitFor({ timeout: 60000 });
	await confirm.check();
	await shot('restore-copy-approval');
	// watch the job service while it runs: which secret does it mount?
	const t0 = Date.now();
	await page.getByRole('button', { name: /^Approve/ }).click();
	log('approved in the UI');
	let jobSecrets = '';
	for (let i = 0; i < 200 && !jobSecrets; i++) {
		jobSecrets = vm(M[0], `for s in $(docker service ls --format '{{.Name}}' --filter label=dbcm.installation=${INST} | grep -i job); do echo "$s: $(docker service inspect -f '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}} {{end}}' $s)"; done`).trim();
		if (!jobSecrets) await sleep(1500);
	}
	log('restore job services and their secrets:\n' + jobSecrets);
	const op = await waitOp(startedBody.id, { until: ['succeeded', 'failed', 'aborted'], timeout: 900, label: 'restore of the copy' }).catch(async (e) => { log(String(e)); return null; });
	await shot('restore-copy-done');
	const ops = (await api('GET', '/operations')).body.items.find((o) => o.id === startedBody.id);
	log('operation record:', redact({ state: ops?.state, reason: ops?.reason, backend: ops?.backend, kind: ops?.kind }));
	const got = await inspectSql(startedBody, CHECK);
	log('restored copy state:', got.replace(/\n/g, ' | '));
	log('source state       :', st.srcCheck.replace(/\n/g, ' | '));
	log('IDENTICAL:', got.trim() === st.srcCheck.trim());
	log('wsrep_on:', await inspectSql(startedBody, 'SELECT @@wsrep_on'));
	log('elapsed s', Math.round((Date.now() - t0) / 1000));
	await close();
};

// Ask for the restore of the (tampered) copy: expected 409 and no operation left behind.
cmds.tamperask = async () => {
	await session();
	const host = (await api('GET', '/hosts')).body.items.find((h) => /m2$/.test(h.hostname)).nodeId;
	const before = (await api('GET', '/operations')).body.items.length;
	const r = await api('POST', '/restores/inspect', { cluster: st.cluster, copy: st.copy, host, ttlMinutes: 30 });
	log('inspect of the tampered copy ->', r.status, redact(r.body));
	const p = await api('POST', `/backup-copies/${st.copy}/restore-proposal`, {});
	log('restore proposal of the tampered copy ->', p.status, redact(p.body).slice(0, 300));
	log('operations before/after:', before, (await api('GET', '/operations')).body.items.length);
	await close();
};


// ------------------------------------------------------------ part 2: archive schedule, PITR over several files, gap
const STATE_SQL = "SET SESSION wsrep_sync_wait=15; SELECT UTC_TIMESTAMP(); SELECT COUNT(*), SUM(amount), SUM(id) FROM e19.orders; SELECT batch, COUNT(*) FROM e19.orders GROUP BY batch ORDER BY batch; CHECKSUM TABLE e19.orders";
const archiveOps = async () => ((await api('GET', '/operations')).body.items ?? []).filter((o) => o.kind === 'dbbackup/binlog-archive').sort((a, b) => String(a.startedAt).localeCompare(String(b.startedAt)));

cmds.archive = async (cycles = '5', every = '*/2 * * * *') => {
	await session();
	const pol = { enabled: true, schedule: '0 4 * * *', archive: { enabled: true, schedule: every } };
	await must('PUT', `/clusters/${st.cluster}/backup-policy`, pol);
	log('policy now:', redact((await api('GET', '/backups')).body.items.find((b) => b.cluster === st.cluster)?.backupPolicy));
	st.points = st.points ?? [];
	const n0 = (await archiveOps()).length;
	const baseline = (await archiveOps()).filter((o) => o.state === 'succeeded').length;
	for (let k = 1; k <= Number(cycles); k++) {
		const base = 100000 * k;
		rsql(1, `USE e19;
INSERT INTO orders SELECT ${base} + seq, 'B${k}', seq % 89, CONCAT('batch-${k}-', seq) FROM seq_1_to_800;
UPDATE orders SET amount = amount + ${k}, note = 'upd' WHERE batch = 'O' AND id % 50 = ${k};
DELETE FROM orders WHERE batch = 'O' AND id % 97 = ${k};`);
		await sleep(2500);
		const out = rsql(1, STATE_SQL).replace(/\t/g, ' ').split('\n');
		const t = out[0].trim().replace(' ', 'T') + 'Z';
		const point = { k, t, state: out.slice(1).join(' | ') };
		st.points.push(point); save();
		log(`state after batch ${k} at ${t}: ${point.state}`);
		await sleep(2500);
		// wait for the next scheduled archive run (a new succeeded one)
		const t0 = Date.now();
		for (;;) {
			const ops = await archiveOps();
			const ok = ops.filter((o) => o.state === 'succeeded').length;
			if (ok >= baseline + k) { log(`archive runs succeeded so far: ${ok - baseline} (latest ${ops.at(-1).id} at ${ops.at(-1).startedAt}); wait ${Math.round((Date.now() - t0) / 1000)} s`); break; }
			const bad = ops.filter((o) => ['failed', 'aborted'].includes(o.state));
			if (bad.length) log('archive run problem:', redact(bad.map((o) => `${o.id} ${o.state} ${o.reason}`)));
			if (Date.now() - t0 > 600000) throw new Error('no archive run within 10 minutes');
			await sleep(10000);
		}
	}
	log('archive operations:', redact((await archiveOps()).map((o) => ({ id: o.id, state: o.state, startedAt: o.startedAt, reason: o.reason, by: o.by }))));
	log('events (archive):', redact(((await api('GET', '/events')).body.items ?? []).filter((e) => /archive|binlog/i.test(JSON.stringify(e))).slice(0, 12)));
	await close();
};

cmds.archivelist = async () => {
	log('archived objects on the main target:\n' + vm(M[0], 'python3 /usr/local/bin/s3tool.py /root/dbcm-e4/backend.json list e19-'));
};

cmds.propose = async (idx) => {
	await session();
	for (const p of idx === undefined ? st.points : [st.points[Number(idx) - 1]]) {
		const r = await api('GET', `/clusters/${st.cluster}/pitr-proposal?at=${encodeURIComponent(p.t)}`);
		log(`pitr-proposal batch ${p.k} at ${p.t} -> ${r.status} ${redact(r.body)}`);
	}
	await close();
};

// PITR restore to point idx (1-based) in a new isolated instance; compares the state with the one recorded at that time.
cmds.pitr = async (idx) => {
	await session();
	const p = st.points[Number(idx) - 1];
	const host = (await api('GET', '/hosts')).body.items.find((h) => /m3$/.test(h.hostname)).nodeId;
	const r = await api('POST', '/restores/inspect', { cluster: st.cluster, pointInTime: p.t, host, ttlMinutes: 60 });
	log(`inspect pointInTime ${p.t} -> ${r.status} ${redact(r.body).slice(0, 300)}`);
	if (r.status !== 202) { await close(); throw new Error('PITR restore refused'); }
	const started = r.body;
	const t0 = Date.now();
	await sleep(3000);
	await must('POST', `/operations/${started.id}/approve`, { digest: started.approval.digest });
	const op = await waitOp(started.id, { timeout: 900, label: 'PITR restore' });
	const got = await inspectSql(started, `SELECT UTC_TIMESTAMP(); SELECT COUNT(*), SUM(amount), SUM(id) FROM e19.orders; SELECT batch, COUNT(*) FROM e19.orders GROUP BY batch ORDER BY batch; CHECKSUM TABLE e19.orders`);
	const gotState = got.split('\n').slice(1).join(' | ');
	log(`restored state:  ${gotState}`);
	log(`recorded state:  ${p.state}`);
	log('IDENTICAL:', gotState === p.state, ' operation', op.state, op.reason ?? '', ' elapsed s', Math.round((Date.now() - t0) / 1000));
	await close();
};


// ------------------------------------------------------------ part 3: fresh-server bootstrap (manager in m2, fresh Debian 12 in m1)
const MGR = 'dbcm-lab-m2';
const bootEnv = () => Object.fromEntries(readFileSync(process.env.E19_BOOT_ENV ?? '.lab/e19-boot.env', 'utf8').trim().split(/\s+/).map((kv) => kv.split('=')));
const bootKey = () => vm(MGR, 'cat /root/dbcm-e4/boot_key');
const bootView = (v) => redact({ status: v.status, error: v.error, hostKey: v.hostKey, progress: (v.progress ?? []).map((p) => `${p.id}=${p.state}${p.message ? ' (' + p.message + ')' : ''}`), mainApproved: v.mainApproved, credentialsHeld: v.credentialsHeld });
async function bootWait(id, until, timeout = 1500) {
	const t0 = Date.now();
	let last = '';
	for (;;) {
		const v = (await api('GET', `/bootstrap/drafts/${id}`)).body;
		const cur = bootView(v);
		if (cur !== last) { log('draft', id, cur); last = cur; }
		if (until.includes(v.status)) return v;
		if (Date.now() - t0 > timeout * 1000) throw new Error('timeout waiting for ' + until);
		await sleep(5000);
	}
}

cmds.bootsetup = async () => {
	const token = vm(MGR, `journalctl -u dbcm-e4-manager --no-pager | grep -o 'setup token[^:]*: [^ ]*' | tail -1 | awk '{print $NF}'`).trim();
	await open();
	await page.goto('/setup');
	await page.fill('#setup-token', token);
	await page.fill('#account-id', 'admin');
	await page.fill('#display-name', 'E19 boot admin');
	await page.getByRole('button', { name: /Register passkey/i }).click();
	await page.waitForSelector('#codes-title', { timeout: 30000 });
	await page.getByLabel(/stored these recovery codes/i).check();
	await page.getByRole('button', { name: /Continue to the manager/i }).click();
	await page.waitForLoadState('networkidle');
	log('administrator created');
	await close();
};

// HTTP API run. profile JSON in argv[0] (default: docker + native agent), host id argv[1].
cmds.bootapi = async (profileJson, hostId = 'fresh1', draftId = hostId) => {
	await session();
	const e = bootEnv();
	log('support matrix:', redact((await api('GET', '/bootstrap/support')).body).slice(0, 200));
	const profile = JSON.parse(profileJson ?? '{"serviceUser":"dbcm","runtime":"docker","nativeAgent":true,"cluster":{"platform":"none"}}');
	const body = { id: draftId, host: { id: hostId, address: e.E19B_FRESH, user: 'root', role: 'node' }, profile, credential: { privateKey: bootKey() } };
	let v = await must('POST', '/bootstrap/drafts', body, [201]);
	log('hostKey', JSON.stringify(v.hostKey));
	v = await must('POST', `/bootstrap/drafts/${draftId}/fingerprint`, { fingerprint: v.hostKey.fingerprint }, [200]);
	const plan = (await api('GET', `/bootstrap/drafts/${draftId}/plan`)).body;
	log('plan: blocked', plan.blocked, 'os', redact(plan.os), 'digest', plan.digest);
	for (const f of plan.findings ?? []) log('finding', f.severity, f.code, f.message);
	for (const s2 of plan.steps ?? []) log('step', s2.id, s2.phase, s2.mutating ? 'mutating' : '', s2.expect ?? '', s2.title);
	if (plan.blocked) { log('plan blocked, nothing approved'); await close(); return; }
	await must('POST', `/bootstrap/drafts/${draftId}/approve`, { digest: plan.digest }, [200]);
	await must('POST', `/bootstrap/drafts/${draftId}/run`, undefined, [202]);
	const end = await bootWait(draftId, ['completed', 'main-complete', 'paused', 'cancelled'], 1500);
	log('final', draftId, bootView(end));
	await close();
};

cmds.bootget = async (id) => { await session(); log(id, bootView((await api('GET', `/bootstrap/drafts/${id}`)).body)); await close(); };
// Join first, sync and enroll in the manager, print the Swarm node ID of the bootstrapped server.
cmds.bootenroll = async () => {
	await session();
	let sw = ((await api('GET', '/swarms')).body.items ?? [])[0];
	if (!sw) sw = await must('POST', '/swarms', { name: 'lab', dockerHost: 'unix:///var/run/docker.sock' });
	await must('POST', `/swarms/${sw.id}/sync`);
	const hosts = (await api('GET', '/hosts')).body.items;
	log('hosts after sync:', JSON.stringify(hosts.map((h) => ({ nodeId: h.nodeId, hostname: h.hostname, role: h.role, enrollment: h.enrollment }))));
	const m1 = hosts.find((h) => /m1$/.test(h.hostname));
	await must('POST', `/hosts/${m1.nodeId}/enroll`, {});
	st.m1Node = m1.nodeId; save();
	log('enrolled host record for the bootstrapped server, node id', m1.nodeId);
	await close();
};
cmds.bootcancel =async (id) => { await session(); await must('POST', `/bootstrap/drafts/${id}/cancel`, undefined, [200, 202, 204]); await close(); };
cmds.bootresume = async (id) => {
	await session();
	await must('POST', `/bootstrap/drafts/${id}/run`, undefined, [202]);
	log('final', bootView(await bootWait(id, ['completed', 'main-complete', 'paused', 'cancelled'], 1500)));
	await close();
};

// The same flow through the UI wizard (page /bootstrap); Swarm worker join when joinToken is given.
cmds.bootui = async (hostId = 'fresh-ui', joinToken = '', managerAddr = '') => {
	await session();
	const e = bootEnv();
	await page.goto('/bootstrap');
	await page.waitForLoadState('networkidle');
	await shot('boot-ui-form');
	await page.fill('#boot-id', hostId);
	await page.fill('#boot-address', e.E19B_FRESH);
	await page.fill('#boot-user', 'root');
	await page.selectOption('#boot-auth', 'key');
	await page.fill('#boot-key', bootKey());
	await page.selectOption('#boot-runtime', 'docker');
	if (joinToken) {
		await page.selectOption('#boot-cluster', 'swarm');
		await page.selectOption('#boot-role', 'join-worker');
		await page.fill('#boot-mgr', managerAddr);
		await page.fill('#boot-join', joinToken);
	}
	await shot('boot-ui-form-filled');
	await page.locator('form button[type=submit]').first().click();
	await page.waitForSelector('#boot-fp', { timeout: 60000 });
	const fp = (await page.locator('code').filter({ hasText: /SHA256:/ }).first().textContent()).trim();
	log('UI shows host key fingerprint', fp);
	await shot('boot-ui-fingerprint');
	await page.fill('#boot-fp', fp);
	await page.locator('form button[type=submit]').first().click();
	const approve = page.getByRole('button', { name: /^Approve/i }).first();
	await approve.waitFor({ timeout: 90000 });
	await shot('boot-ui-plan');
	await approve.click();
	await page.getByRole('button', { name: /^(Run|Start)/i }).first().click();
	const end = await bootWait(hostId, ['completed', 'main-complete', 'paused', 'cancelled'], 1500);
	await page.waitForTimeout(2500);
	await shot('boot-ui-done');
	log('final (UI)', hostId, bootView(end));
	await close();
};

// sh VM FILE: run a local script as root in a lab VM (stdout is printed).
cmds.sh = async (vmName, file) => {
	try { log(`sh ${vmName} ${file}:\n` + vm(vmName, 'sh -s 2>&1', readFileSync(file, 'utf8'))); } catch (e) { log('sh failed:', String(e.stdout ?? '').slice(-1500)); throw new Error('script failed'); }
};

const cmd = process.argv[2];
if (!cmds[cmd]) { console.error('commands:', Object.keys(cmds).join(' ')); process.exit(2); }
export { cmds, session, close, api, must, waitOp, rsql, vm, log, shot, st, save, sleep, M, page as _page, CLUSTER, INST, BASE };
try {
	await cmds[cmd](...process.argv.slice(3));
} catch (e) {
	log('FAILED:', e.stack ?? e);
	await shot('failure-' + cmd).catch(() => {});
	await browser?.close().catch(() => {});
	process.exit(1);
}
