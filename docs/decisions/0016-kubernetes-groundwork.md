# ADR 0016: Kubernetes renderer and registration groundwork

Status: **Proposal**, 2026-10-24. W10 is fakes-only groundwork, not R9 delivery.

## Context and confirmed boundaries

[X02, X03 and X12](../KONZEPT.md#scope-extension) require platform choice,
reviewable generated configuration and existing ownership/secret safeguards.
[ADR 0012](0012-broadened-scope-platform.md) keeps Galera and R1 first.
Kubernetes timing and supported distributions remain owner decisions in
[ENTSCHEIDUNGEN.md](../ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension).
The [R9 gates](../RELEASES.md#r9--kubernetes-back-end-and-cluster-provisioning)
are unchanged. This work does not provision hosts or contact any cluster.

## Proposed mechanism

`service.Orchestrator` renders existing service plans. `service.Swarm` preserves
Swarm artifacts; `service/k8s.Renderer` emits a typed, deliberately limited API
subset. This is a rendering boundary, not yet a deployment/reconciliation
interface. The existing agent, Swarm runner and Galera safety planners are
unchanged. Explicit `Enabled` flags are required for the renderer and KubeAPI;
`api.Config.ExperimentalKubernetes` and a registration backend are both required
for HTTP registration. The shipping manager constructs neither backend nor
Kubernetes client, and exposes no Kubernetes execution/provisioning route.

The renderer emits Deployment or StatefulSet, internal/headless Service,
PVC/local PV, Secret file references, ConfigMap and Traefik IngressRoute shapes.
It retains source blockers and adds blockers for differing platform semantics:
overlay networks, published ports, restart/update policy, security context,
placement translation, config target mapping and unsupported Traefik features.
No generated Secret contains credential data. Secret refs expect a pre-existing
Secret with key `value`; ConfigMap file refs expect key `content`.

Object ordering and map encoding are deterministic. Galera definition previews
use deterministic preview identities; they are not deployment identities.
The common digest binds the source plan, platform, artifacts and blockers.
Export is a YAML 1.2 stream of pretty-printed JSON documents with `---`
boundaries. It is a review artifact; applying an export manually would bypass
the manager's blockers and is outside this groundwork.

Ownership uses `dbcm.installation`, `dbcm.cluster`, `dbcm.service-type`,
`dbcm.service-name` and `dbcm.workload` labels, with `dbcm.io/groundwork` and
`dbcm.io/platform` annotations. Kubernetes API mutations additionally require an
explicit resource/namespace/name inventory and matching UID. List sends an
installation selector and independently filters results against labels, scope
and UID inventory. Delete reads the object and submits UID/resourceVersion
preconditions. Apply reads and checks an existing object, then uses server-side
apply with UID/resourceVersion and `force=false`. A missing name is first
created atomically with POST, so a racing foreign object produces a conflict
instead of being adopted by apply. A successful create remains inventoried if
the subsequent apply fails. Inventory is in memory; durable inventory and
unknown-outcome reconciliation are prerequisites for production use.

The minimal REST adapter uses a caller-supplied TLS client or Go's default trust
store, an HTTPS origin, a wrapped bearer token, bounded responses, timeouts and
no redirects. Errors exclude remote bodies and transport details. There is no
client-go dependency, API discovery, watch, pagination, token refresh or
kubeconfig interpreter. Unsupported paginated lists fail closed. Stored
kubeconfigs are opaque and never run exec/auth-provider plugins.

Registration has only GET/POST `/api/v1/kubernetes-clusters`, guarded by the new
global `kubernetes_cluster` authz resource (administrator preset; custom grants
possible). Session, Origin/CSRF and audit behavior comes from the existing API
middleware. The model accepts exactly one token or kubeconfig (32 KiB maximum).
The manager backend seals credential and metadata in one existing AES-GCM
metastore document, bound to collection/key. Create uses version-zero CAS to
prevent duplicate/concurrent registration from overwriting credentials. Public
responses contain only metadata and credential kind/presence. Registration
checks syntax, not connectivity or credential validity.

## Alternatives still open

| Choice | Benefits | Costs and evidence needed |
| --- | --- | --- |
| client-go in manager | Mature discovery, informers, auth support and API types | Larger dependency surface, manager egress and credential privileges; requires leader fencing, version matrix and credential threat review |
| In-cluster agent | Narrow manager connectivity and cluster-scoped credential placement; fits the existing agent boundary | Agent deployment/enrollment/upgrade and least-privilege RBAC design; partition and duplicate-controller fencing required |
| Minimal REST in manager (current experiment) | Small dependency surface and easily inspected requests | Limited API subset; no evidence for production compatibility, watches, token rotation or retries; not selected as the production architecture |
| StatefulSet plus local PV for Galera (preview only) | Stable workload names and explicit node-local volume affinity | StatefulSet ordering is not quorum control. Need identity markers, host-return checks, fenced bootstrap, `safe_to_bootstrap`, donor health and recovery approval. Local-node loss does not relocate data |
| CSI/network storage for Galera | Could simplify rescheduling | Changes the confirmed local-storage model; needs owner decision, durability/performance and fencing evidence |
| DBCM CRDs/operator | Native desired state, status and reconciliation | CRD lifecycle, conversion, schema and controller privileges add complexity; cluster/controller ownership must be resolved |
| No DBCM CRD (current preview) | Native objects with manager inventory; smaller installation footprint | Manager must implement observation, drift and recovery safely. Traefik IngressRoute still needs Traefik's third-party CRD/controller |

No alternative is approved as the production implementation by this ADR.

## Verification and limits

Tests cover typed render shapes, determinism/digest changes, local PV affinity
and retention, Secret refs, ConfigMaps, IngressRoute, Galera bootstrap removal
and semantic blockers; fake KubeAPI ownership; REST HTTP requests through
`httptest.ResponseRecorder` and an in-memory transport; registration authz,
disabled behavior and credential-free responses; sealed storage, AAD binding
and concurrent registration CAS. These HTTP tests open no sockets and do not
verify TLS handshakes or a Kubernetes server's schema/SSA behavior.

See [STATUS](../STATUS.md#w10-kubernetes-groundwork) for actual command results,
including sandbox restrictions. No cluster, VM, controller, real token or
production kubeconfig was used. Before any real test: settle timing and initial
platform, review credential/RBAC scope, design durable inventory and fencing,
validate manifests against a chosen Kubernetes/Traefik version, and define a
budgeted lab with scoped cleanup. Galera deployment/recovery remains blocked.

## API references consulted

- [Kubernetes server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
- [Kubernetes local volumes and required node affinity](https://kubernetes.io/docs/concepts/storage/volumes/#local)
- [StatefulSet identity and limitations](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)
