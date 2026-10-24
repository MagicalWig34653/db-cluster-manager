import { afterEach, describe, expect, it, vi } from 'vitest';
import { createClient, isServiceInfo, isServicePlan, serviceTypes, type ServiceInput } from './api';
import { buildServiceInput, downloadPlan, canUpdateService, defaultStrategy, emptyServiceForm, planExport, planView, serviceHealth, validateServiceForm } from './services';
import { emptyDetails, validateDetails } from './service-details';
import { mockServicePlan, mockUpdatePlan } from './mock-services';
import { mockFetch } from './mock';

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

const storage = (type: 'sftp' | 'ftp' | 's3' | 'nfs') => {
 const f = { ...emptyServiceForm(type), swarm: 's', name: 'files', image: 'example.invalid/server:1', hostId: 'node-1' };
 Object.assign(f.details, { rootUserSecret: 'root-user', rootPasswordSecret: 'root-pw', passiveAddress: 'files.example.invalid', certSecret: 'tls-cert', keySecret: 'tls-key' });
 f.details.users[0] = { name: 'alice', uid: '1000', gid: '1000', home: '/alice', readOnly: false, passwordSecret: 'alice-pw', authorizedKeysSecret: 'alice-keys' };
 f.details.exports[0].clients = '192.0.2.0/24,198.51.100.1';
 return f;
};
const input: ServiceInput = { swarm: 's', name: 'web', type: 'container', spec: { image: 'nginx:1.27', replicas: 1, placement: {} } };

describe('typed service forms', () => {
 it('offers exactly the seven backend types', () => expect(serviceTypes).toEqual(['container', 'traefik', 'galera', 'sftp', 'ftp', 's3', 'nfs']));
 it.each(['sftp', 'ftp', 's3', 'nfs'] as const)('builds %s with numbers, host and resource units', type => {
  const f = storage(type);
  expect(validateServiceForm(f)).toEqual([]);
  const r = buildServiceInput(f);
  expect(r.type).toBe(type);
  expect(r.spec).toMatchObject({ image: f.image, placement: { hostId: 'node-1' }, resources: { limits: { milli_cpu: 500, memory_bytes: 256 * 1048576 } } });
  expect(validateServiceForm({ ...f, hostId: '' })).toContain('details');
 });
 it('encodes only secret references and numeric users; FTP omits SSH keys', () => {
  expect(buildServiceInput(storage('sftp')).spec.users).toEqual([{ name: 'alice', uid: 1000, gid: 1000, home: '/alice', readOnly: false, passwordSecret: 'alice-pw', authorizedKeysSecret: 'alice-keys' }]);
  expect(JSON.stringify(buildServiceInput(storage('ftp')))).not.toContain('authorizedKeysSecret');
 });
 it('keeps SFTP internal without a portMode and NFS clients as arrays', () => {
  const f = storage('sftp'); f.details.publishedPort = '0';
  expect(buildServiceInput(f).spec).not.toHaveProperty('portMode');
  expect(buildServiceInput(storage('nfs')).spec.exports).toEqual([{ path: '/shared', clients: ['192.0.2.0/24', '198.51.100.1'], readOnly: false, squash: 'root_squash' }]);
 });
 it('requires explicit plaintext opt-in, valid passive range and both FTPS secrets', () => {
  const d = storage('ftp').details;
  expect(validateDetails('ftp', { ...d, keySecret: '' }, 'n')).toBe(false);
  expect(validateDetails('ftp', { ...d, passiveMax: '30100' }, 'n')).toBe(false);
  expect(validateDetails('ftp', { ...d, mode: 'ftp' }, 'n')).toBe(false);
  expect(validateDetails('ftp', { ...d, mode: 'ftp', allowPlaintext: true }, 'n')).toBe(true);
 });
 it('rejects malformed numeric values and wildcard NFS clients', () => {
  const f = storage('nfs'); f.details.exports[0].clients = '*';
  expect(validateServiceForm(f)).toContain('details');
  expect(validateDetails('sftp', { ...storage('sftp').details, memoryMiB: 'NaN' }, 'n')).toBe(false);
 });
 it('builds Galera with the cluster contract and no single-host placement', () => {
  const f = { ...emptyServiceForm('galera'), swarm: 's', name: 'db', image: 'mariadb:11.4' };
  expect(validateServiceForm(f)).toContain('details');
  f.details.network = 'db-net'; f.details.rootPasswordSecret = 'db-root';
  expect(validateServiceForm(f)).toEqual([]);
  expect(buildServiceInput(f).spec).toMatchObject({ size: 3, network: 'db-net', rootPasswordSecret: 'db-root', placement: {}, autoReplace: false });
  expect(mockServicePlan('create', buildServiceInput(f)).requiresApproval).toBe(false);
 });
});

describe('health and update availability', () => {
 const now = Date.parse('2026-10-24T12:00:00Z');
 it.each(['healthy', 'degraded', 'unhealthy', 'unknown'] as const)('preserves fresh %s observations', state => {
  expect(serviceHealth({ state, running: 1, desired: 1, observedAt: new Date(now).toISOString() }, now)).toBe(state);
 });
 it('never treats absent, malformed or stale observations as healthy', () => {
  expect(serviceHealth()).toBe('unknown');
  expect(serviceHealth({ state: 'unavailable', running: 0, desired: 3, observedAt: new Date(now).toISOString() }, now)).toBe('unhealthy');
  for (const observedAt of [undefined, 'bad', new Date(now - 120001).toISOString()]) expect(serviceHealth({ state: 'healthy', running: 1, desired: 1, observedAt }, now)).toBe('unknown');
 });
 it('offers only supported types and prevents overlapping updates', () => {
  const s = { ...input, id: 'id', createOperation: 'completed-create' };
  expect(canUpdateService(s)).toBe(true);
  for (const type of ['galera', 'sftp', 'ftp', 's3', 'nfs']) expect(canUpdateService({ ...s, type })).toBe(false);
  expect(canUpdateService({ ...s, updateOperation: 'pending' })).toBe(false);
  expect(canUpdateService({ ...s, deleteOperation: 'pending' })).toBe(false);
 });
 it('validates optional health and rejects malformed expert and rollback data', () => {
  expect(isServiceInfo({ ...input, id: 'id', health: { state: 'healthy', running: '1', desired: 1 } })).toBe(false);
  const p = mockUpdatePlan(input, { spec: input.spec, strategy: defaultStrategy() });
  expect(isServicePlan(p)).toBe(true);
  expect(isServicePlan({ ...p, agentSteps: [{ id: 'x', preconditions: 'bad' }] })).toBe(false);
  expect(isServicePlan({ ...p, rollback: {} })).toBe(false);
 });
});

describe('mock plans and export', () => {
 it('shows warnings and blocks NFS, required digests and canaries', () => {
  const nfs = mockServicePlan('create', buildServiceInput(storage('nfs')));
  expect(nfs.warnings.length).toBeGreaterThan(0); expect(planView(nfs).canStart).toBe(false); expect(nfs.agentSteps).toEqual([]);
  const f = storage('s3'); f.details.requireDigest = true;
  expect(planView(mockServicePlan('create', buildServiceInput(f))).canStart).toBe(false);
  expect(planView(mockUpdatePlan(input, { spec: input.spec, strategy: { ...defaultStrategy(), canary: { replicas: 1, holdSeconds: 30 } } })).canStart).toBe(false);
 });
 it('keeps the prepared rollback and expert fields intact in JSON and YAML 1.2 exports', () => {
  const plan = mockUpdatePlan(input, { spec: { ...input.spec, image: 'nginx:1.28' }, strategy: defaultStrategy() });
  expect(plan.rollback?.kind).toBe('update'); expect(plan.agentSteps?.[0].command).toBe('update-service');
  expect(JSON.parse(planExport(plan, 'json'))).toEqual(JSON.parse(JSON.stringify(plan)));
  expect(JSON.parse(planExport(plan, 'yaml').replace('%YAML 1.2\n---\n', ''))).toEqual(JSON.parse(planExport(plan, 'json')));
 });
 it('requests update and rollback plans and applies the update only after approval', async () => {
  vi.stubGlobal('location', { href: 'http://localhost/' });
  const c = createClient(mockFetch);
  const body = { spec: { image: 'nginx:1.29', replicas: 2, placement: {}, resources: { limits: {}, reservations: {} } }, strategy: defaultStrategy() };
  const p = await c.previewServiceUpdate('mock-svc-1', body); expect(p.kind).toBe('update');
  const started = await c.updateService('mock-svc-1', body);
  expect((await c.services()).find(s => s.id === 'mock-svc-1')?.spec.image).toBe('nginx:1.27');
  await c.approveOperation(started.operation.id, started.approval.digest);
  expect((await c.services()).find(s => s.id === 'mock-svc-1')?.spec.image).toBe('nginx:1.29');
  const rb = await c.previewServiceUpdate('mock-svc-1', { rollback: true, strategy: defaultStrategy() });
  expect(rb.artifacts[0].content).toContain('nginx:1.27');
  await expect(c.previewServiceUpdate('mock-galera', { rollback: true, strategy: defaultStrategy() })).rejects.toMatchObject({ status: 409 });
 });
 it('downloads both formats with their MIME types and revokes object URLs', () => {
  vi.useFakeTimers();
  const link = { href: '', download: '', click: vi.fn() };
  vi.stubGlobal('document', { createElement: () => link });
  const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-plan');
  const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  const plan = mockServicePlan('create', input);
  downloadPlan(plan, 'json');
  expect(link.download).toBe('service-create-plan.json');
  expect(create.mock.calls[0][0]).toMatchObject({ type: 'application/json' });
  downloadPlan(plan, 'yaml');
  expect(link.download).toBe('service-create-plan.yaml');
  expect(create.mock.calls[1][0]).toMatchObject({ type: 'application/yaml' });
  expect(link.click).toHaveBeenCalledTimes(2);
  vi.runAllTimers(); expect(revoke).toHaveBeenCalledTimes(2);
 });
 it('encodes IDs and sends the exact strategy/rollback request', async () => {
  const calls: { url: string; body: unknown }[] = [];
  const c = createClient((async (url, init) => { calls.push({ url: String(url), body: JSON.parse(String(init?.body)) }); return new Response(JSON.stringify(mockUpdatePlan(input, { spec: input.spec, strategy: defaultStrategy() }))); }) as typeof fetch);
  const body = { rollback: true, strategy: { ...defaultStrategy(), order: 'start-first' as const, parallelism: 2, delaySeconds: 4, monitorSeconds: 60, maxFailureRatio: 0.2, onFailure: 'pause' as const } };
  await c.previewServiceUpdate('a/b', body);
  expect(calls).toEqual([{ url: '/api/v1/services/a%2Fb/update-plan', body }]);
 });
});
