import { describe, expect, it } from 'vitest';
import { isModuleInfo } from './api';
import { modulesMock } from './mock-modules';
import { navItems } from './components/app/NavList.svelte';

describe('modules', () => {
	it('accepts the mock catalog and rejects a malformed module', async () => {
		const { items } = await modulesMock('GET', '/api/v1/modules', {})!.json();
		expect(items.length).toBeGreaterThan(0);
		expect(items.every(isModuleInfo)).toBe(true);
		expect(isModuleInfo({ ...items[0], enabled: 'yes' })).toBe(false);
	});

	it('refuses to disable a module that another enabled module needs', () => {
		expect(modulesMock('PUT', '/api/v1/modules/image-build', { enabled: false })!.status).toBe(409);
		expect(modulesMock('PUT', '/api/v1/modules/image-composer', { enabled: false })!.status).toBe(200);
		expect(modulesMock('PUT', '/api/v1/modules/image-build', { enabled: false })!.status).toBe(200);
	});

	it('only gates navigation entries by modules that exist', async () => {
		const { items } = await modulesMock('GET', '/api/v1/modules', {})!.json();
		const ids = new Set(items.map((m: { id: string }) => m.id));
		for (const i of navItems) if (i.module) expect(ids.has(i.module)).toBe(true);
	});
});
