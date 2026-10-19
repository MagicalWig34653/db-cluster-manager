<script lang="ts">
	import { getHealth, getVersion, type VersionInfo } from '../lib/api';

	type State<T> = { state: 'loading' } | { state: 'error'; message: string } | { state: 'ok'; value: T };

	let health = $state<State<{ status: string }>>({ state: 'loading' });
	let info = $state<State<VersionInfo>>({ state: 'loading' });

	$effect(() => {
		const toErr = (e: unknown) => ({ state: 'error' as const, message: e instanceof Error ? e.message : String(e) });
		getHealth().then((value) => (health = { state: 'ok', value }), (e) => (health = toErr(e)));
		getVersion().then((value) => (info = { state: 'ok', value }), (e) => (info = toErr(e)));
	});
</script>

<h1 class="mb-1 text-2xl font-semibold">Manager</h1>
<p class="mb-6 text-sm text-zinc-600 dark:text-zinc-400">
	Prototype. No Swarm is connected and no cluster operations exist yet; only the manager process is reported.
</p>

<dl class="grid grid-cols-[10rem_1fr] gap-y-2 rounded-lg border border-zinc-200 p-4 text-sm dark:border-zinc-800">
	<dt class="text-zinc-500">API health</dt>
	<dd>
		{#if health.state === 'loading'}Checking…{:else if health.state === 'error'}Unavailable ({health.message}){:else}{health.value.status}{/if}
	</dd>
	{#if info.state === 'ok'}
		<dt class="text-zinc-500">Version</dt><dd>{info.value.version}</dd>
		<dt class="text-zinc-500">Commit</dt><dd>{info.value.commit}</dd>
		<dt class="text-zinc-500">Go</dt><dd>{info.value.goVersion}</dd>
		<dt class="text-zinc-500">Platform</dt><dd>{info.value.platform}</dd>
	{:else if info.state === 'error'}
		<dt class="text-zinc-500">Version</dt><dd>Unavailable ({info.message})</dd>
	{/if}
</dl>
