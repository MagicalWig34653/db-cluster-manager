import { describe, expect, it } from 'vitest';
import { isServicePlan, type ServiceInput } from './api';
import { mockServicePlan } from './mock-services';
import { planExport } from './services';
const input = (type: ServiceInput['type']): ServiceInput => ({ swarm: 'mock-swarm', name: 'files', type, spec: { image: 'example.invalid/server:1', users: [{ name: 'alice', passwordSecret: 'password-ref' }], routers: [{ name: 'web' }] } });
describe('owned Swarm config artifacts', () => {
 it.each(['sftp', 'ftp', 'traefik'] as const)('shows %s creation before service and cleanup after removal', type => {
  const plan = mockServicePlan('create', input(type));
  expect(isServicePlan(plan)).toBe(true);
  const config = plan.steps.find(s => s.kind === 'create-config')!;
  expect(config).toBeDefined();
  expect(plan.artifacts.some(a => a.name === config.artifact)).toBe(true);
  expect(plan.steps.at(-1)?.after).toContain(config.id);
  expect(plan.agentSteps?.some(s => s.command === 'create-config')).toBe(true);
  expect(JSON.parse(planExport(plan, 'json')).steps).toContainEqual(config);
  expect(mockServicePlan('delete', input(type)).steps.find(s => s.kind === 'remove-config')?.after).toEqual(['remove-service']);
 });
 it('does not create externally referenced SFTP configs', () => {
  const i = input('sftp'); i.spec.usersConfig = 'existing-users';
  expect(mockServicePlan('create', i).steps.some(s => s.kind === 'create-config')).toBe(false);
 });
 it('rejects malformed config fields instead of rendering them', () => {
  const p = mockServicePlan('create', input('sftp'));
  expect(isServicePlan({ ...p, steps: [{ id: 'c', kind: 'create-config', config: {} }] })).toBe(false);
  expect(isServicePlan({ ...p, steps: [{ id: 'c', kind: 'create-config', after: 'bad' }] })).toBe(false);
 });
});
