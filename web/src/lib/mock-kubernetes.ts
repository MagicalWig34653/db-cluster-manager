import type { KubernetesRegistration, KubernetesRegistrationInput } from './api';
import { validRegistration } from './kubernetes';
const registrations: KubernetesRegistration[] = [{ id: 'mock-k8s', name: 'Mock Kubernetes', endpoint: 'https://kube.example.invalid:6443', namespace: 'default', credentialKind: 'token', credentialSet: true }];
export function kubernetesMock(method: string, path: string, input: KubernetesRegistrationInput): Response | undefined {
 if (path !== '/api/v1/kubernetes-clusters') return;
 const json = (status: number, value: unknown) => new Response(JSON.stringify(value), { status });
 if (method === 'GET') return json(200, registrations);
 if (method !== 'POST') return;
 const kind = input.token ? 'token' : 'kubeconfig';
 if (!!input.token === !!input.kubeconfig || !validRegistration({ ...input, kind, credential: input[kind] ?? '' })) return json(400, { error: 'bad_request' });
 if (registrations.some(r => r.id === input.id)) return json(409, { error: 'conflict' });
 // Store metadata only; never retain mock credentials.
 const view: KubernetesRegistration = { id: input.id, name: input.name, endpoint: input.endpoint, namespace: input.namespace, credentialKind: kind, credentialSet: true };
 registrations.push(view);
 return json(201, view);
}
