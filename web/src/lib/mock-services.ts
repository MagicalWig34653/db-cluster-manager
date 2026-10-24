// Invented plans for the development mock server; no Docker calls or real execution.
import type { ServiceInput, ServicePlan, ServiceUpdateInput } from './api';
export function mockServicePlan(kind: 'create' | 'delete', input: ServiceInput, removeVolumes = false): ServicePlan {
 const spec = input.spec;
 const storage = ['sftp', 'ftp', 's3', 'nfs'].includes(input.type);
 const blockers: string[] = [];
 const warnings: string[] = [];
 if (kind === 'create') {
  if (input.type === 'traefik' && Array.isArray(spec.routers) && spec.routers.length) blockers.push('Mock: dynamic configuration cannot be delivered as a Swarm config yet.');
  if (input.type === 'nfs') blockers.push('Mock: kernel nfsd and CAP_SYS_ADMIN are unavailable through the Swarm agent contract.');
  if (storage) {
   if (spec.requireDigest && !String(spec.image).includes('@sha256:')) blockers.push('Mock: requireDigest needs an image digest.');
   else if (!String(spec.image).includes('@sha256:')) warnings.push('Mock: image is pinned by tag only.');
   warnings.push('Mock: local service data has no backup.');
   if (input.type === 'sftp' && !spec.usersConfig) warnings.push('Mock: users file must be supplied as an existing Swarm config.');
   if (input.type === 'sftp' && !spec.hostKeySecret) warnings.push('Mock: without a host key secret the SSH identity may change on restart.');
   if (input.type === 'ftp' && spec.mode === 'ftp') warnings.push('Mock: FTP sends credentials without encryption.');
   if (input.type === 's3') warnings.push('Mock: single drive, no replication or erasure coding; published endpoints have no TLS.');
   if (input.type === 's3' && spec.consolePublishedPort) warnings.push('Mock: console is exposed directly.');
   if (input.type === 'nfs' && JSON.stringify(spec.exports).includes('no_root_squash')) warnings.push('Mock: no_root_squash grants remote root privileges.');
  }
 } else if (removeVolumes) warnings.push('Mock: data volumes will be deleted irreversibly.');
 const steps = kind === 'delete' ? [{ id: 'remove-service', kind: 'remove-service' }, ...(removeVolumes ? [{ id: 'remove-volumes', kind: 'remove-volume' }] : [])] : [
  ...(storage ? [{ id: 'create-volume', kind: 'create-volume', note: 'Mock: identity-guarded local volume' }] : []),
  { id: 'create-service', kind: input.type === 'galera' ? 'create-cluster' : 'create-service', note: 'Mock: ' + input.name, conditions: ['owned resources only'] }
 ];
 const executable = blockers.length === 0;
 return {
  kind, name: input.name, type: input.type, steps, warnings, blockers, executable,
  requiresApproval: !(kind === 'create' && input.type === 'galera'),
  artifacts: kind === 'delete' ? [] : [{ name: 'mock-desired-spec.json', mediaType: 'application/json', content: JSON.stringify(spec, null, 2) }],
  agentSteps: executable ? steps.map(s => ({ id: s.id, command: s.kind, target: String((spec.placement as { hostId?: string })?.hostId ?? 'mock-host-1'), args: { mock: true, spec }, preconditions: ['mock ownership gate'] })) : [],
  provisionalIds: true,
  text: kind === 'delete' ? 'Mock removal plan; data is kept unless explicitly selected.' : 'Mock creation plan — illustrative steps only; the real manager supplies the exact operation.'
 };
}
export function mockUpdatePlan(input: ServiceInput, body: ServiceUpdateInput, previous?: Record<string, unknown>): ServicePlan {
 const target = body.rollback ? previous : body.spec;
 const plan = mockServicePlan('create', { ...input, spec: target ?? input.spec });
 plan.kind = 'update'; plan.requiresApproval = true;
 if (!['container', 'traefik'].includes(input.type)) plan.blockers.push('Mock: this type has no generic update executor.');
 if (body.rollback && !previous) plan.blockers.push('Mock: no previous spec is available.');
 if (body.strategy.canary) plan.blockers.push('Mock: no canary executor exists.');
 if (body.strategy.order === 'start-first' && Array.isArray(input.spec.volumes) && input.spec.volumes.length) plan.blockers.push('Mock: start-first cannot share a local volume.');
 for (const key of ['replicas', 'placement', 'volumes']) if (target && JSON.stringify(target[key]) !== JSON.stringify(input.spec[key])) plan.blockers.push('Mock: immutable field ' + key);
 plan.steps = ['update-service', 'await-service', 'check-health'].map(kind => ({ id: kind, kind, conditions: kind === 'check-health' ? ['all desired tasks running'] : [] }));
 plan.executable = plan.blockers.length === 0;
 plan.agentSteps = plan.executable ? plan.steps.map(s => ({ id: s.id, command: s.kind === 'check-health' ? undefined : s.kind, target: 'mock-host-1', args: { mock: true, spec: target, strategy: body.strategy }, preconditions: s.conditions ?? [] })) : [];
 plan.text = 'Mock rolling update; review the prepared rollback plan and approve separately.';
 plan.rollback = { ...plan, rollback: undefined, text: 'Mock prepared rollback: separate request and approval required.', artifacts: [], agentSteps: plan.agentSteps?.map(s => ({ ...s, args: { mock: true, spec: input.spec, strategy: body.strategy } })) };
 return plan;
}
