// The optional-module state of this installation, from GET /modules. The
// navigation hides entries of modules that are not enabled; nothing is
// shown for a module until the answer arrived.

import { api, type ModuleInfo } from './api';

let list = $state<ModuleInfo[]>([]);

export const modules = {
	get list() {
		return list;
	},
	/** True when the module exists and runs. */
	enabled: (id: string) => list.some((m) => m.id === id && m.enabled),
	set(m: ModuleInfo) {
		list = list.map((x) => (x.id === m.id ? m : x));
	},
	async refresh() {
		try {
			list = (await api.modules()).items;
		} catch {
			// Without an answer no optional entry is offered.
			list = [];
		}
	}
};
