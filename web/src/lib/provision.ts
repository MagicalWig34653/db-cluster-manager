import type { ProvisionPlanInput } from './api';

export function validProvision(input: ProvisionPlanInput): boolean {
	if (!/^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$/.test(input.id) || input.hosts.length < 1 || input.hosts.length > 16 || input.hosts[0].role !== 'manager') return false;
	if (input.hosts.filter((h) => h.role === 'manager').length % 2 === 0) return false;
	const ids = new Set<string>();
	for (const h of input.hosts) {
		if (ids.has(h.id) || ![h.id, h.address, h.user].every((v) => /^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$/.test(v))) return false;
		ids.add(h.id);
		const c = input.credentials[h.id];
		if (!c?.privateKey.trim() || !c.knownHosts.trim()) return false;
	}
	return true;
}
