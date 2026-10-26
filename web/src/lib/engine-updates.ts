import type { EngineUpdatePlan } from './api';

export function canExportEnginePlan(plan: EngineUpdatePlan | undefined, target: string, confirmedManager: boolean): boolean {
	return !!plan && plan.exportable && plan.approvalRequired && plan.targetVersion === target &&
		plan.blockers.length === 0 && (!plan.pinnedManager || confirmedManager);
}

export function engineRunbookFilename(host: string): string {
	return `cluster-manager-engine-update-${host.replace(/[^A-Za-z0-9_-]/g, '_')}.sh`;
}
