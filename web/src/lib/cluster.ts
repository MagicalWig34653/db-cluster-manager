// Cluster creation form model: validation, request building and a UI-side
// capacity estimate. The request shape is manager.CreateClusterRequest
// (internal/manager/operations.go); resources and placement use galera's
// JSON names (milli_cpu, memory_bytes). The manager plans authoritatively;
// the estimate here is only a preview and ignores load placed by other
// clusters (the host list has no allocation field).

import type { CreateClusterRequest, Host } from './api';
import type { MessageKey } from './i18n';

export const placementModes = ['automatic', 'exclude', 'prefer', 'allowlist'] as const;
export type PlacementMode = (typeof placementModes)[number];

export interface ClusterForm {
	swarm: string;
	name: string;
	size: 3 | 5;
	image: string;
	network: string;
	rootPasswordSecret: string;
	gcacheSize: string;
	mode: PlacementMode;
	hosts: string[]; // host IDs for exclude/prefer/allowlist
	cpuLimit: string; // CPUs, e.g. "2" or "1.5"
	memLimit: string; // GiB
	cpuReservation: string;
	memReservation: string;
	reserveCpu: string; // host reserve
	reserveMem: string;
	autoReplace: boolean;
	binlog: boolean;
}

export const emptyForm = (): ClusterForm => ({
	swarm: '',
	name: '',
	size: 3,
	image: '',
	network: '',
	rootPasswordSecret: '',
	gcacheSize: '',
	mode: 'automatic',
	hosts: [],
	cpuLimit: '',
	memLimit: '',
	cpuReservation: '',
	memReservation: '',
	reserveCpu: '',
	reserveMem: '',
	autoReplace: false,
	binlog: false
});

const GiB = 1024 ** 3;

/** Parses a non-negative decimal; empty means unset (0). NaN on garbage. */
export function parseAmount(s: string): number {
	const v = s.trim();
	if (v === '') return 0;
	if (!/^\d+([.,]\d+)?$/.test(v)) return NaN;
	return Number(v.replace(',', '.'));
}

export const milliCpu = (cpus: number) => Math.round(cpus * 1000);
export const memBytes = (gib: number) => Math.round(gib * GiB);

export type FormError = { field: keyof ClusterForm; key: MessageKey };

export function validate(f: ClusterForm): FormError[] {
	const errs: FormError[] = [];
	if (!f.swarm) errs.push({ field: 'swarm', key: 'valRequired' });
	if (!/^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$/.test(f.name)) errs.push({ field: 'name', key: 'valClusterName' });
	if (!f.image.trim()) errs.push({ field: 'image', key: 'valRequired' });
	if (!f.network.trim()) errs.push({ field: 'network', key: 'valRequired' });
	if (!f.rootPasswordSecret.trim()) errs.push({ field: 'rootPasswordSecret', key: 'valRequired' });
	const amounts = {
		cpuLimit: f.cpuLimit,
		memLimit: f.memLimit,
		cpuReservation: f.cpuReservation,
		memReservation: f.memReservation,
		reserveCpu: f.reserveCpu,
		reserveMem: f.reserveMem
	} as const;
	for (const [k, v] of Object.entries(amounts)) {
		if (Number.isNaN(parseAmount(v))) errs.push({ field: k as keyof ClusterForm, key: 'valNumber' });
	}
	const lc = parseAmount(f.cpuLimit);
	const rc = parseAmount(f.cpuReservation);
	if (lc > 0 && rc > lc) errs.push({ field: 'cpuReservation', key: 'valReservationAboveLimit' });
	const lm = parseAmount(f.memLimit);
	const rm = parseAmount(f.memReservation);
	if (lm > 0 && rm > lm) errs.push({ field: 'memReservation', key: 'valReservationAboveLimit' });
	// The planner counts the larger of limit and reservation and refuses a zero
	// demand (found in the RC browser run: a bare 400 "bad_request").
	if (!Number.isNaN(lc + rc) && Math.max(lc, rc) <= 0) errs.push({ field: 'cpuLimit', key: 'valDemandRequired' });
	if (!Number.isNaN(lm + rm) && Math.max(lm, rm) <= 0) errs.push({ field: 'memLimit', key: 'valDemandRequired' });
	if (f.mode !== 'automatic' && f.hosts.length === 0) errs.push({ field: 'hosts', key: 'valPickHosts' });
	if (f.mode === 'allowlist' && f.hosts.length < f.size) errs.push({ field: 'hosts', key: 'valAllowlistTooSmall' });
	return errs;
}

/** Builds the POST /api/v1/clusters body. Call only when validate() is empty. */
export function buildRequest(f: ClusterForm): CreateClusterRequest {
	const amt = (cpu: string, mem: string) => {
		const out: { milli_cpu?: number; memory_bytes?: number } = {};
		const c = parseAmount(cpu);
		const m = parseAmount(mem);
		if (c > 0) out.milli_cpu = milliCpu(c);
		if (m > 0) out.memory_bytes = memBytes(m);
		return out;
	};
	const placement: CreateClusterRequest['placement'] = { reserve: amt(f.reserveCpu, f.reserveMem) };
	if (f.mode === 'exclude') placement.excluded = [...f.hosts];
	if (f.mode === 'prefer') placement.preferred = [...f.hosts];
	if (f.mode === 'allowlist') placement.allowlist = [...f.hosts];
	return {
		swarm: f.swarm,
		name: f.name,
		size: f.size,
		image: f.image.trim(),
		network: f.network.trim(),
		rootPasswordSecret: f.rootPasswordSecret.trim(),
		...(f.gcacheSize.trim() ? { gcacheSize: f.gcacheSize.trim() } : {}),
		resources: { limits: amt(f.cpuLimit, f.memLimit), reservations: amt(f.cpuReservation, f.memReservation) },
		placement,
		...(f.autoReplace ? { autoReplace: true } : {}),
		...(f.binlog ? { binlog: true } : {})
	};
}

export type HostVerdict =
	| { id: string; eligible: true; note?: MessageKey }
	| { id: string; eligible: false; reason: MessageKey };

/**
 * Mirrors the hard placement rules of galera.PlacementPolicy for display:
 * enrolled, ready, active, not a spare, known capacity covering demand plus
 * reserve, and the allowlist/exclusion rules. Hosts with unknown fields are
 * rejected, never assumed fine.
 */
export function preflight(f: ClusterForm, hosts: Host[]): { verdicts: HostVerdict[]; eligible: number; enough: boolean } {
	const demandCpu = milliCpu(Math.max(parseAmount(f.cpuLimit) || 0, parseAmount(f.cpuReservation) || 0));
	const demandMem = memBytes(Math.max(parseAmount(f.memLimit) || 0, parseAmount(f.memReservation) || 0));
	const resCpu = milliCpu(parseAmount(f.reserveCpu) || 0);
	const resMem = memBytes(parseAmount(f.reserveMem) || 0);
	const verdicts: HostVerdict[] = hosts
		.filter((h) => h.swarm === f.swarm)
		.map((h): HostVerdict => {
			const id = h.nodeId;
			if (h.enrollment !== 'enrolled') return { id, eligible: false, reason: 'pfNotEnrolled' };
			if (f.mode === 'exclude' && f.hosts.includes(id)) return { id, eligible: false, reason: 'pfExcluded' };
			if (f.mode === 'allowlist' && !f.hosts.includes(id)) return { id, eligible: false, reason: 'pfNotAllowlisted' };
			if (h.state !== 'ready') return { id, eligible: false, reason: 'pfNotReady' };
			if (h.availability !== 'active') return { id, eligible: false, reason: 'pfNotActive' };
			if (h.spare) return { id, eligible: false, reason: 'pfSpare' };
			if (!h.nanoCpus || !h.memoryBytes) return { id, eligible: false, reason: 'pfCapacityUnknown' };
			if (h.nanoCpus / 1e6 < demandCpu + resCpu || h.memoryBytes < demandMem + resMem)
				return { id, eligible: false, reason: 'pfNoCapacity' };
			return { id, eligible: true };
		});
	const eligible = verdicts.filter((v) => v.eligible).length;
	return { verdicts, eligible, enough: eligible >= f.size };
}
