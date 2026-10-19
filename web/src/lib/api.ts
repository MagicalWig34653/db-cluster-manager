export interface VersionInfo {
	component: string;
	version: string;
	commit: string;
	goVersion: string;
	platform: string;
}

async function getJSON<T>(path: string, fetchFn: typeof fetch): Promise<T> {
	const res = await fetchFn(path, { headers: { Accept: 'application/json' } });
	if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
	return (await res.json()) as T;
}

export const getHealth = (f: typeof fetch = fetch) => getJSON<{ status: string }>('/api/v1/health', f);
export const getVersion = (f: typeof fetch = fetch) => getJSON<VersionInfo>('/api/v1/version', f);
