import { describe, expect, it } from 'vitest';
import { getHealth, getVersion } from './api';

const respond = (status: number, body: unknown) =>
	(async () => new Response(JSON.stringify(body), { status })) as unknown as typeof fetch;

describe('api client', () => {
	it('parses health', async () => {
		expect(await getHealth(respond(200, { status: 'ok' }))).toEqual({ status: 'ok' });
	});
	it('rejects non-2xx responses instead of inventing data', async () => {
		await expect(getVersion(respond(503, {}))).rejects.toThrow('HTTP 503');
	});
});
