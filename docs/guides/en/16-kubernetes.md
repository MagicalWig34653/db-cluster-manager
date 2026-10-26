# Kubernetes groundwork (W10)

This is **experimental groundwork, fakes only**. Kubernetes is not a supported
deployment target yet. No R9 exit gate is met. Kubernetes timing and the first
supported distribution remain open owner decisions. Galera/R1 keeps priority.
See [ADR 0016](../../decisions/0016-kubernetes-groundwork.md).

The shipping manager does not enable or wire this feature. Library consumers
must explicitly enable the renderer/KubeAPI. An API test host must set
`api.Config.ExperimentalKubernetes` and supply the registration backend; the
backend also requires `Enabled`. There is no production configuration switch,
CLI, executable provisioning, deployment or Kubernetes recovery workflow in W10. A separate [flag-gated k3s script preview](19-provisioning.md) exists; no host is changed by it.

## Registration UI

Open **Kubernetes registrations** in the navigation. The page lists metadata and
credential presence, and registers an existing cluster with an ID, display name,
HTTPS API origin (no trailing slash), namespace and either a bearer token or an
opaque kubeconfig. IDs and namespaces use lowercase DNS names (up to 63 characters).
Credentials are write only, limited to 32 KiB and cleared after each send attempt.
Changing the credential type clears the field too. No credential is stored in
browser storage. A successful registration means metadata was saved; it does
not establish connectivity, compatibility or deployment readiness.

The development mock provides synthetic registrations without cluster contact.
The shipping manager still returns 503 because its registration backend is not
wired. Loading, permission, disabled-feature and request errors remain visible;
there is no deployment or connectivity-test button. Browser rendering and a
real-manager integration run of this page remain unverified.

The only HTTP contract is registration:

- `POST /api/v1/kubernetes-clusters`: `id`, `name`, an HTTPS origin `endpoint`,
  `namespace`, and exactly one `token` or opaque `kubeconfig` (up to 32 KiB).
- `GET /api/v1/kubernetes-clusters`: metadata, credential kind and presence.

Both require a full session and permission on global `kubernetes_cluster`;
registration additionally requires the usual same-origin request and create
permission. Read permission is required to list. Only the administrator preset
includes these permissions. Disabled/unwired endpoints answer 503 after access
checks. The manager storage adapter seals credentials with the existing data
keyring; they never appear in responses. Registration does not contact a
cluster, test credentials or execute kubeconfig plugins. Use synthetic values
in tests; do not enter production credentials.

A service definition can produce a deterministic preview, digest and YAML
export through `service/k8s.Renderer.Definition`. The export uses JSON-style
YAML documents. It shows generated workloads, Services, local PV/PVC,
ConfigMaps, Secret references and supported Traefik route shapes. It has no
execution approval route. Keep every blocker visible; exporting a plan does
not authorize applying it with external tools.

Local storage needs an explicit Kubernetes hostname, absolute data path and
capacity for each volume. PVs retain data and bind to a specific node and
claim. Swarm node IDs are not Kubernetes hostnames. Identity-marker validation
and returning-node recovery are still blocked. Galera StatefulSets are only
shapes for review: bootstrap, `safe_to_bootstrap`, quorum, donor and guided
recovery gates are not implemented for Kubernetes. Traefik IngressRoute needs
an installed controller and its CRD; unsupported TLS/middleware/backend
semantics block the plan.

No real cluster was used. Tests use fake objects, a fake HTTP transport with
`httptest` and an in-process replicated metastore. Actual verification and
sandbox limits are in [STATUS](../../STATUS.md#w10-kubernetes-groundwork).
