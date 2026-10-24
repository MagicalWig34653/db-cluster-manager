// Typed form state for the strict specs in internal/service. Secret fields hold names only.
import type { ServiceType } from './api';
import type { MessageKey } from './i18n';
export const emptyDetails = () => ({
 size: '3', network: '', rootPasswordSecret: '', gcacheSize: '128M', autoReplace: false,
 requireDigest: false, memoryMiB: '256', cpuMilli: '500', networks: '',
 publishedPort: '2222', portMode: 'host', usersConfig: '', hostKeySecret: '',
 users: [{ name: '', uid: '1000', gid: '1000', home: '', readOnly: false, passwordSecret: '', authorizedKeysSecret: '' }],
 mode: 'ftps-explicit', allowPlaintext: false, controlPort: '21', passiveMin: '30000', passiveMax: '30009', passiveAddress: '', certSecret: '', keySecret: '',
 rootUserSecret: '', region: 'us-east-1', domain: '', apiPublishedPort: '9000', console: false, consolePublishedPort: '0',
 exports: [{ path: '/shared', clients: '', readOnly: false, squash: 'root_squash' }]
});
export type ServiceDetails = ReturnType<typeof emptyDetails>;
type TextKey = { [K in keyof ServiceDetails]: ServiceDetails[K] extends string ? K : never }[keyof ServiceDetails];
export interface DetailField { key: TextKey; label: MessageKey; number?: boolean; min?: number; max?: number }
const field = (key: TextKey, label: MessageKey, number = false, min = 0, max = 65535): DetailField => ({ key, label, number, min, max });
export function detailFields(type: ServiceType): DetailField[] {
 const base = [field('cpuMilli', 'svcCpu', true, 1, 1000000), field('memoryMiB', 'svcMemory', true, 4, 1048576), field('networks', 'svcNetworks')];
 switch (type) {
 case 'galera': return [field('size', 'svcSize', true, 3, 50), field('network', 'svcNetwork'), field('rootPasswordSecret', 'svcRootPassword'), field('gcacheSize', 'svcGcache'), ...base.slice(0, 2)];
 case 'sftp': return [...base, field('publishedPort', 'svcPublishedPort', true), field('usersConfig', 'svcUsersConfig'), field('hostKeySecret', 'svcHostKey')];
 case 'ftp': return [...base, field('controlPort', 'svcControlPort', true), field('passiveMin', 'svcPassiveMin', true, 1), field('passiveMax', 'svcPassiveMax', true, 1), field('passiveAddress', 'svcPassiveAddress'), field('certSecret', 'svcCertSecret'), field('keySecret', 'svcKeySecret')];
 case 's3': return [...base, field('rootUserSecret', 'svcRootUser'), field('rootPasswordSecret', 'svcRootPassword'), field('region', 'svcRegion'), field('domain', 'svcDomain'), field('apiPublishedPort', 'svcApiPort', true), field('consolePublishedPort', 'svcConsolePort', true)];
 case 'nfs': return [...base, field('publishedPort', 'svcPublishedPort', true)];
 default: return [];
 }
}
const split = (s: string) => s.split(/[,\s]+/).filter(Boolean);
const integer = (s: string, min: number, max = 65535) => /^\d+$/.test(s) && Number(s) >= min && Number(s) <= max;
export function validateDetails(type: ServiceType, d: ServiceDetails, host: string): boolean {
 if (detailFields(type).some(f => f.number && !integer(d[f.key], f.min ?? 0, f.max))) return false;
 if (type === 'galera') return !!d.network && !!d.rootPasswordSecret;
 if (!host) return false;
 if (type === 'sftp' || type === 'ftp') {
  if (new Set(d.users.map(u => u.name)).size !== d.users.length || new Set(d.users.map(u => u.uid)).size !== d.users.length) return false;
  if (!d.users.length || d.users.some(u => !/^[a-z_][a-z0-9_-]{0,31}$/.test(u.name) || !integer(u.uid, 1000, 60000) || !integer(u.gid, 0, 60000) || (u.home !== '' && (!u.home.startsWith('/') || u.home === '/' || u.home.includes('..'))) || (!u.passwordSecret && !(type === 'sftp' && u.authorizedKeysSecret)))) return false;
 }
 if (type === 'ftp') return !!d.passiveAddress && Number(d.passiveMax) >= Number(d.passiveMin) && Number(d.passiveMax) - Number(d.passiveMin) < 100 && (d.mode === 'ftp' ? d.allowPlaintext : !!d.certSecret && !!d.keySecret);
 if (type === 's3') return !!d.rootUserSecret && !!d.rootPasswordSecret && (d.console || Number(d.consolePublishedPort) === 0);
 if (type === 'nfs') return d.exports.length > 0 && d.exports.every(e => e.path.startsWith('/') && !e.path.includes('..') && !!e.clients && !e.clients.includes('*'));
 return true;
}
export function buildDetails(type: ServiceType, d: ServiceDetails, image: string, hostId: string): Record<string, unknown> {
 const resources = { limits: { milli_cpu: Number(d.cpuMilli), memory_bytes: Number(d.memoryMiB) * 1048576 }, reservations: {} };
 if (type === 'galera') return { size: Number(d.size), image, network: d.network.trim(), rootPasswordSecret: d.rootPasswordSecret.trim(), gcacheSize: d.gcacheSize, resources, placement: {}, autoReplace: d.autoReplace };
 const base = { image, placement: { hostId }, resources, networks: split(d.networks), requireDigest: d.requireDigest };
 const users = d.users.map(u => ({ name: u.name, uid: Number(u.uid), gid: Number(u.gid), home: u.home, readOnly: u.readOnly, ...(u.passwordSecret ? { passwordSecret: u.passwordSecret } : {}), ...(type === 'sftp' && u.authorizedKeysSecret ? { authorizedKeysSecret: u.authorizedKeysSecret } : {}) }));
 const port = Number(d.publishedPort);
 switch (type) {
 case 'sftp': return { ...base, publishedPort: port, ...(port ? { portMode: d.portMode } : {}), users, usersConfig: d.usersConfig, hostKeySecret: d.hostKeySecret };
 case 'ftp': return { ...base, users, mode: d.mode, allowPlaintext: d.allowPlaintext, controlPort: Number(d.controlPort), passive: { min: Number(d.passiveMin), max: Number(d.passiveMax) }, passiveAddress: d.passiveAddress, portMode: d.portMode, certSecret: d.certSecret, keySecret: d.keySecret };
 case 's3': return { ...base, rootUserSecret: d.rootUserSecret, rootPasswordSecret: d.rootPasswordSecret, region: d.region, domain: d.domain, apiPublishedPort: Number(d.apiPublishedPort), console: d.console, consolePublishedPort: Number(d.consolePublishedPort), portMode: d.portMode };
 case 'nfs': return { ...base, publishedPort: port, exports: d.exports.map(e => ({ ...e, clients: split(e.clients) })) };
 default: return base;
 }
}
