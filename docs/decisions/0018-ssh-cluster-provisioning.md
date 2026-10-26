# ADR 0018: SSH cluster provisioning plans

Status: **Experimental proposal**, 2026-10-25. No real SSH host or cluster was contacted.

## Context

[X02, X03 and X12](../KONZEPT.md#scope-extension) call for optional creation of a Swarm or Kubernetes cluster on supplied hosts with visible commands, permissions and ownership. The [R9 gates](../RELEASES.md#r9--kubernetes-back-end-and-cluster-provisioning) remain open. [ADR 0016](0016-kubernetes-groundwork.md) covers an unrelated Kubernetes workload renderer and metadata registration.

## Decision for this experimental increment

The manager can create a Swarm bootstrap plan from SSH hosts. It performs read-only preflight for Linux, required free ports, synchronized time, at least 10 GiB free disk and absence of Swarm/k3s membership. It refuses any existing cluster. Plans carry ordered, exportable per-host scripts, observations and a SHA-256 digest. Join tokens and private keys are absent from the scripts and operation definition. Every SSH key and pinned OpenSSH `known_hosts` value is sealed in a replicated document. No SSH agent, password login or trust-on-first-use is used.

Swarm preflight also requires an installed, responding Docker daemon; this step does not install or update Docker.

The API is disabled by default (`manager.experimentalProvisioning`). Plan and execute require global settings `operate` permission with the normal session, same-origin and audit guards. Execute compares the plan digest and stores a runner operation. Every mutating step waits for the existing operation approval endpoint to accept the digest of the stored definition. The manager's term-fenced runner carries the operation across leader changes. The command adapter retrieves sealed credentials, checks host state again, fetches a join token from the seed manager per attempt and passes it through SSH stdin. Confirmed successful steps enter a replicated ownership manifest with the observed Swarm cluster ID. Later cleanup must use this manifest and must be a separate reviewed plan.

The Kubernetes variant needs a second flag (`manager.experimentalKubernetesProvisioning`) and renders k3s script templates only. It requires a separately reviewed local k3s installer path and never executes through SSH. Its argument and environment-variable shape follows the [official k3s installation documentation](https://docs.k3s.io/installation/configuration); the templates themselves were not run. It does not settle Kubernetes distribution support or R9 timing. The flag is a preview gate, not a support claim.

## Alternatives and tradeoffs

| Alternative | Benefit | Cost / reason not selected yet |
| --- | --- | --- |
| Agent bootstrap on each host | Existing mTLS and fencing after enrollment | Requires a trusted initial install and enrollment channel on empty hosts |
| SSH with pinned host keys (current experiment) | No initial agent; uses operator-supplied hosts and keys | Manager needs highly privileged keys; remote command completion can be uncertain; host-key rotation is manual |
| Cloud-specific API provisioning | Better machine lifecycle and identity on one provider | Does not cover user-supplied machines and adds provider-specific credentials |
| kubeadm for Kubernetes | Upstream primitives | Certificate distribution, CNI, etcd topology and upgrade policy need decisions and evidence |
| k3s templates (preview only) | Small readable command shape | Installer provenance, service topology and version pinning have not been selected or tested |

## Failure behavior and limits

A host that already belongs to a cluster is refused even when its identity appears plausible. If an SSH mutating command might have completed but the answer or manifest write is lost, the runner records an unknown outcome and requires reconciliation; it must not silently adopt that host. A confirmed manifest entry may be replayed only while the observed cluster ID still matches. Preflight is a point-in-time observation; it cannot reserve ports or disk. The present port probe and OS coverage are narrow and need lab validation. It does not install Docker, configure firewalls or NTP, rotate SSH keys, clean up a cluster or automatically register the new Swarm for management. The Swarm join process exposes its short-lived token to the remote Docker process arguments; this needs a threat review before production use. The experimental manager flag should only be used in an isolated lab.

Fakes cover deterministic plans, foreign-cluster refusal, token handling, sealed drafts, ownership replay, API authz and UI flows. The SSH transport itself and real Docker/k3s commands were not exercised. See [STATUS](../STATUS.md) and [the bilingual guide](../guides/en/19-provisioning.md). The next gate is a budgeted, owned lab run after the SSH and installer threat review, with deliberate disconnect/restart cases and scoped cleanup.
