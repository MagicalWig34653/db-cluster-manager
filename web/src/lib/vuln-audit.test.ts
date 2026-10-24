import { describe, expect, it } from 'vitest';
import { createClient, type VulnAuditRecord } from './api';
import { auditRecords } from './vuln';
import { vulnMock } from './mock-vuln';
const record = (seq: number, extra: Partial<VulnAuditRecord> = {}): VulnAuditRecord => ({ id: String(seq), seq, at: '2026-10-24T00:00:00Z', actor: 'admin', event: 'policy_changed', result: 'ok', ...extra });
describe('global vulnerability audit', () => {
 it('includes global events and searches references without mutating the response', () => {
  const records = [record(1), record(3, { provider: 'local-model' }), record(2, { findingId: 'finding-2' })];
  expect(auditRecords(records, '').map(r => r.seq)).toEqual([3, 2, 1]);
  expect(auditRecords(records, ' LOCAL-MODEL ').map(r => r.seq)).toEqual([3]);
  expect(auditRecords(records, 'finding-2').map(r => r.seq)).toEqual([2]);
  expect(auditRecords(records, 'missing')).toEqual([]);
  expect(records.map(r => r.seq)).toEqual([1, 3, 2]);
 });
 it('loads the existing global audit contract from the mock', async () => {
  const client = createClient(async (url, init) => vulnMock(init?.method ?? 'GET', String(url), {}) ?? new Response('', { status: 404 }));
  const records = await client.vulnAudit();
  expect(records.length).toBeGreaterThan(0);
 });
});
