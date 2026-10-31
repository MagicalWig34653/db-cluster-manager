import { describe, expect, it } from 'vitest';
import { createClient, isPointInTimeProposal, type PointInTimeProposal } from './api';
import { formatUtc, pitrReady, utcInputToRfc3339 } from './pitr';

const proposal = (over: Partial<PointInTimeProposal> = {}): PointInTimeProposal => ({
	cluster: 'c1',
	at: '2026-10-22T01:30:00Z',
	covered: true,
	backup: 'b1',
	segments: 2,
	approvalRequired: true,
	inPlace: false,
	...over
});

describe('point-in-time input', () => {
	it('reads the datetime-local value as UTC and rejects what is not a time', () => {
		expect(utcInputToRfc3339('2026-10-22T01:30')).toBe('2026-10-22T01:30:00Z');
		expect(utcInputToRfc3339('2026-10-22T01:30:15')).toBe('2026-10-22T01:30:15Z');
		expect(utcInputToRfc3339(' 2026-10-22T23:59:59 ')).toBe('2026-10-22T23:59:59Z');
		for (const bad of ['', '2026-10-22', '2026-10-22 01:30', '2026-02-30T01:30', '2026-10-22T25:00', 'tomorrow', '2026-10-22T01:30Z']) {
			expect(utcInputToRfc3339(bad)).toBeUndefined();
		}
	});
	it('shows UTC explicitly', () => {
		expect(formatUtc('2026-10-22T01:30:00Z')).toBe('2026-10-22 01:30:00 UTC');
		expect(formatUtc('2026-10-22T01:30:00.250Z')).toBe('2026-10-22 01:30:00 UTC');
	});
	it('starts only for the checked time and only when covered', () => {
		expect(pitrReady('2026-10-22T01:30:00Z', proposal())).toBe(true);
		expect(pitrReady('2026-10-22T01:30:01Z', proposal())).toBe(false);
		expect(pitrReady('2026-10-22T01:30:00Z', proposal({ covered: false, reason: 'gap' }))).toBe(false);
		expect(pitrReady(undefined, proposal())).toBe(false);
		expect(pitrReady('2026-10-22T01:30:00Z', undefined)).toBe(false);
	});
});

describe('point-in-time API', () => {
	const server = (status: number, body: unknown) => {
		const calls: { url: string; init: RequestInit }[] = [];
		const fetchImpl = (async (url: string, init: RequestInit) => {
			calls.push({ url: String(url), init });
			return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
		}) as typeof fetch;
		return { client: createClient(fetchImpl), calls };
	};
	it('validates the proposal, covered or not', () => {
		expect(isPointInTimeProposal(proposal())).toBe(true);
		expect(
			isPointInTimeProposal(
				proposal({ covered: false, reason: 'the archive ends at 02:00', coverage: { segments: 2, from: '2026-10-22T00:00:00Z', until: '2026-10-22T01:00:00Z', gap: 'sequence 3 is missing after 2' } })
			)
		).toBe(true);
		expect(isPointInTimeProposal({ ...proposal(), covered: 'yes' })).toBe(false);
		expect(isPointInTimeProposal({ ...proposal(), coverage: { segments: 'two', from: '2026-10-22T00:00:00Z' } })).toBe(false);
		expect(isPointInTimeProposal({ ...proposal(), at: 'noon' })).toBe(false);
	});
	it('reads the proposal with the time as a query parameter', async () => {
		const s = server(200, proposal());
		expect(await s.client.pitrProposal('c 1', '2026-10-22T01:30:00Z')).toMatchObject({ covered: true, backup: 'b1' });
		expect(s.calls[0].url).toBe('/api/v1/clusters/c%201/pitr-proposal?at=2026-10-22T01%3A30%3A00Z');
		expect(s.calls[0].init.method).toBe('GET');
		await expect(server(200, { cluster: 'c1' }).client.pitrProposal('c1', '2026-10-22T01:30:00Z')).rejects.toMatchObject({ kind: 'invalid_response' });
	});
	it('starts the binary log archive and sends pointInTime with the restore', async () => {
		let s = server(202, { id: 'op-a', kind: 'dbbackup/binlog-archive' });
		expect(await s.client.startBinlogArchive('c1')).toEqual({ id: 'op-a', kind: 'dbbackup/binlog-archive' });
		expect(s.calls[0].url).toBe('/api/v1/clusters/c1/binlog-archives');
		expect(s.calls[0].init.method).toBe('POST');
		const started = {
			id: 'op', kind: 'k', session: 's', backup: 'b1', host: 'n1', service: 'svc', port: 3307, expiresAt: '2026-10-20T14:00:00Z',
			approval: { digest: 'd'.repeat(64) }, inspectionAccount: 'inspect', inspectionPassword: 'pw', pointInTime: '2026-10-22T01:30:00Z'
		};
		s = server(202, started);
		expect(await s.client.inspectRestore({ cluster: 'c1', host: 'n1', pointInTime: '2026-10-22T01:30:00Z' })).toMatchObject({ pointInTime: '2026-10-22T01:30:00Z' });
		expect(JSON.parse(String(s.calls[0].init.body))).toEqual({ cluster: 'c1', host: 'n1', pointInTime: '2026-10-22T01:30:00Z' });
		await expect(server(202, { ...started, pointInTime: 'noon' }).client.inspectRestore({ cluster: 'c1' })).rejects.toMatchObject({ kind: 'invalid_response' });
	});
});
