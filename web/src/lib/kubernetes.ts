import type { KubernetesRegistrationInput } from './api';

export interface RegistrationForm {
 id: string;
 name: string;
 endpoint: string;
 namespace: string;
 kind: 'token' | 'kubeconfig';
 credential: string;
}
export const emptyRegistration = (): RegistrationForm => ({ id: '', name: '', endpoint: '', namespace: 'default', kind: 'token', credential: '' });
const name = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
export function validRegistration(f: RegistrationForm): boolean {
 let origin = false;
 try {
  const u = new URL(f.endpoint);
  // Check the raw origin too: URL normalizes trailing slashes, paths and whitespace.
  origin = u.protocol === 'https:' && !!u.hostname && !u.username && !u.password &&
   /^https:\/\/[^/?#\s]+$/.test(f.endpoint);
 } catch { /* Invalid URL is a form error. */ }
 return name.test(f.id) && name.test(f.namespace) && f.name.length > 0 && new TextEncoder().encode(f.name).length <= 100 && origin &&
  f.credential.length > 0 && new TextEncoder().encode(f.credential).length <= 32768;
}
export function registrationInput(f: RegistrationForm): KubernetesRegistrationInput {
 return { id: f.id, name: f.name, endpoint: f.endpoint, namespace: f.namespace, [f.kind]: f.credential };
}
