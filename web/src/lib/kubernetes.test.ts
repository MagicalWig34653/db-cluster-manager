import { describe, expect, it, vi, afterEach } from 'vitest';
import { createClient, isKubernetesRegistration } from './api';
import { emptyRegistration, registrationInput, validRegistration } from './kubernetes';
import { mockFetch } from './mock';
const form = { ...emptyRegistration(), id: 'test-k8s', name: 'Test', endpoint: 'https://kube.example.invalid:6443', credential: 'invented-token' };
afterEach(() => vi.unstubAllGlobals());
describe('Kubernetes metadata registration', () => {
 it('validates origin, names and byte limits without connecting', () => {
  expect(validRegistration(form)).toBe(true);
  for (const endpoint of ['http://host', 'https://host/', 'https://host/path', 'https://user:pass@host', 'https://host?q=a', 'https://host#x', ' https://host']) expect(validRegistration({ ...form, endpoint })).toBe(false);
  for (const id of ['Upper', '-a', 'a-', 'a.b', 'a'.repeat(64)]) expect(validRegistration({ ...form, id })).toBe(false);
  expect(validRegistration({ ...form, name: 'ü'.repeat(51) })).toBe(false);
  expect(validRegistration({ ...form, credential: 'ü'.repeat(16385) })).toBe(false);
  expect(validRegistration({ ...form, credential: '' })).toBe(false);
 });
 it('sends exactly the selected credential and rejects credential-bearing responses', () => {
  expect(registrationInput(form)).toMatchObject({ token: 'invented-token' });
  expect(registrationInput({ ...form, kind: 'kubeconfig' })).not.toHaveProperty('token');
  const view = { id: 'k', name: 'K', endpoint: form.endpoint, namespace: 'default', credentialKind: 'token', credentialSet: true };
  expect(isKubernetesRegistration(view)).toBe(true);
  expect(isKubernetesRegistration({ ...view, token: 'secret' })).toBe(false);
  expect(isKubernetesRegistration({ ...view, credentialKind: 'password' })).toBe(false);
 });
 it('registers mock metadata, never returns credentials and refuses duplicate IDs', async () => {
  vi.stubGlobal('location', { href: 'http://localhost/' });
  const api = createClient(mockFetch);
  const result = await api.registerKubernetes(registrationInput(form));
  expect(result.credentialSet).toBe(true);
  expect(JSON.stringify(await api.kubernetesRegistrations())).not.toContain('invented-token');
  await expect(api.registerKubernetes(registrationInput(form))).rejects.toMatchObject({ status: 409 });
 });
 it('uses the existing API route and handles disabled and malformed APIs', async () => {
  const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'unavailable' }), { status: 503 }));
  await expect(createClient(fetcher).kubernetesRegistrations()).rejects.toMatchObject({ status: 503 });
  expect(fetcher.mock.calls[0][0]).toBe('/api/v1/kubernetes-clusters');
  const malformed = createClient(async () => new Response(JSON.stringify([{ id: 'partial' }])));
  await expect(malformed.kubernetesRegistrations()).rejects.toMatchObject({ kind: 'invalid_response' });
 });
});
