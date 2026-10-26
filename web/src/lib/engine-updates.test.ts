import { describe, expect, it } from 'vitest';
import { canExportEnginePlan, engineRunbookFilename } from './engine-updates';
import type { EngineUpdatePlan } from './api';

const plan: EngineUpdatePlan = { swarm: 's', host: 'h', currentVersion: '29.8.0', targetVersion: '29.9.1',
	availableVersions: ['29.9.1'], maintenance: { host: 'h', end: false, safe: true, clusters: [] },
	steps: [], warnings: [], blockers: [], rollbackNotes: [], pinnedManager: false, approvalRequired: true,
	exportable: true, digest: 'sha256:a' };

describe('engine update export guard', () => {
	it('requires the displayed target and an exportable plan', () => {
		expect(canExportEnginePlan(plan, '29.9.1', false)).toBe(true);
		expect(canExportEnginePlan(plan, '30.0.0', true)).toBe(false);
		expect(canExportEnginePlan({ ...plan, blockers: ['quorum'] }, '29.9.1', true)).toBe(false);
		expect(canExportEnginePlan(undefined, '29.9.1', true)).toBe(false);
	});
	it('requires manager confirmation and makes a safe filename', () => {
		expect(canExportEnginePlan({ ...plan, pinnedManager: true }, '29.9.1', false)).toBe(false);
		expect(canExportEnginePlan({ ...plan, pinnedManager: true }, '29.9.1', true)).toBe(true);
		expect(engineRunbookFilename('node/a')).toBe('cluster-manager-engine-update-node_a.sh');
	});
});
