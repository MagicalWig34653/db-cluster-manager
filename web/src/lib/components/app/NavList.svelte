<script lang="ts" module>
	import LayoutDashboard from '@lucide/svelte/icons/layout-dashboard';
	import Boxes from '@lucide/svelte/icons/boxes';
	import Database from '@lucide/svelte/icons/database';
	import Server from '@lucide/svelte/icons/server';
	import HardDrive from '@lucide/svelte/icons/hard-drive';
	import Archive from '@lucide/svelte/icons/archive';
	import Layers from '@lucide/svelte/icons/layers';
	import Hammer from '@lucide/svelte/icons/hammer';
	import Blocks from '@lucide/svelte/icons/blocks';
	import ShieldAlert from '@lucide/svelte/icons/shield-alert';
	import PackageOpen from '@lucide/svelte/icons/package-open';
	import ChartColumn from '@lucide/svelte/icons/chart-column';
	import ListChecks from '@lucide/svelte/icons/list-checks';
	import Activity from '@lucide/svelte/icons/activity';
	import Bell from '@lucide/svelte/icons/bell';
	import Settings from '@lucide/svelte/icons/settings';
	import CircleDashed from '@lucide/svelte/icons/circle-dashed';
	import type { MessageKey } from '../../i18n';

	/** `module` names the optional module (internal/modules) the page belongs to. */
	export const navItems: { href: string; label: MessageKey; icon: typeof Settings; available: boolean; module?: string }[] = [
		{ href: '/', label: 'navOverview', icon: LayoutDashboard, available: true },
		{ href: '/kubernetes', label: 'kubeTitle', icon: Boxes, available: true, module: 'kubernetes' },
		{ href: '/bootstrap', label: 'navBootstrap', icon: Server, available: true, module: 'bootstrap' },
		{ href: '/provision', label: 'provisionTitle', icon: Server, available: true, module: 'provisioning' },
		{ href: '/swarms', label: 'navSwarms', icon: Boxes, available: true },
		{ href: '/clusters', label: 'navClusters', icon: Database, available: true },
		{ href: '/hosts', label: 'navHosts', icon: Server, available: true },
		{ href: '/engine-updates', label: 'engineTitle', icon: Server, available: true },
		{ href: '/databases', label: 'navDatabases', icon: HardDrive, available: true },
		{ href: '/services', label: 'navServices', icon: Layers, available: true },
		{ href: '/images', label: 'navImages', icon: Hammer, available: true, module: 'image-build' },
		{ href: '/image-composer', label: 'navComposer', icon: Blocks, available: true, module: 'image-composer' },
		{ href: '/vulnerabilities', label: 'navVulnerabilities', icon: ShieldAlert, available: true, module: 'vuln' },
		{ href: '/registries', label: 'navRegistries', icon: PackageOpen, available: true },
		{ href: '/backups', label: 'navBackups', icon: Archive, available: true },
		{ href: '/dashboards', label: 'navDashboards', icon: ChartColumn, available: true },
		{ href: '/operations', label: 'navOperations', icon: ListChecks, available: true },
		{ href: '/events', label: 'navEvents', icon: Activity, available: true },
		{ href: '/notifications', label: 'navNotifications', icon: Bell, available: true },
		{ href: '/settings', label: 'navSettings', icon: Settings, available: true }
	];

	export function isCurrent(href: string, pathname: string) {
		return href === '/' ? pathname === '/' : pathname === href || pathname.startsWith(href + '/');
	}
</script>

<script lang="ts">
	import { page } from '$app/state';
	import { t } from '../../locale.svelte';
	import { cn } from '../../utils';
	import { modules } from '../../modules.svelte';

	let { onnavigate }: { onnavigate?: () => void } = $props();
</script>

<nav aria-label={t('mainNavigation')}>
	<ul class="flex flex-col gap-0.5">
		{#each navItems.filter((i) => !i.module || modules.enabled(i.module)) as item (item.href)}
			{@const current = isCurrent(item.href, page.url.pathname)}
			<li>
				<a
					href={item.href}
					aria-current={current ? 'page' : undefined}
					onclick={onnavigate}
					class={cn(
						'flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm outline-none transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:ring-2 focus-visible:ring-ring',
						current ? 'bg-primary/10 font-medium text-foreground' : 'text-muted-foreground'
					)}
				>
					<item.icon class={cn('size-4 shrink-0', current && 'text-primary')} aria-hidden="true" />
					<span class="flex-1">{t(item.label)}</span>
					{#if !item.available}
						<span title={t('notAvailableTitle')}>
							<CircleDashed class="size-3.5 text-muted-foreground" aria-hidden="true" />
							<span class="sr-only">({t('notAvailableTitle')})</span>
						</span>
					{/if}
				</a>
			</li>
		{/each}
	</ul>
</nav>
