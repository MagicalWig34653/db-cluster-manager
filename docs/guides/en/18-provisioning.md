# Provision a cluster (experimental)

**[Fakes]** W10 adds a Swarm host plan and an SSH execution path, but no real SSH host, Docker Swarm or Kubernetes cluster was contacted. Do not use this on production hosts. [ADR 0018](../../decisions/0018-ssh-cluster-provisioning.md) describes the safety model and open work.

The administrator enables `manager.experimentalProvisioning` in the managed manager configuration. This exposes **Provision a cluster** and `/api/v1/provision/plans`. The default is disabled (503). A separate `manager.experimentalKubernetesProvisioning` flag permits a k3s script preview; Kubernetes SSH execution remains disabled. Authentication and the global settings `operate` permission are required.

1. Provide dedicated empty Linux hosts reachable by SSH. The first must be a manager; use an odd manager count. Enter a unique plan ID, hostname or IPv4 address, SSH user, private key and a pinned OpenSSH `known_hosts` entry for every host. Keys are write-only in the form and sealed in the replicated manager store.
2. Run preflight. It checks Linux, free Swarm ports, synchronized time, at least 10 GiB free disk and existing Swarm/k3s membership. A member of any existing cluster is refused. A passing response contains checks, ordered commands, per-host downloadable scripts and a SHA-256 plan digest. No token or private key is exported. Scripts use `JOIN_TOKEN` and `MANAGER_ADDRESS` placeholders; supply them securely if running scripts manually.

For Swarm, Docker must already be installed and its daemon must answer preflight. This increment does not install or update Docker.
3. Review the commands and digest. For Swarm, queue the operation with that digest. Open **Operations** and approve the operation's separate definition digest. Until approval, no host command runs. The runner records steps and can take over after a manager restart. Uncertain SSH outcomes pause for manual reconciliation rather than adopting an existing host.
4. The manager retains an ownership manifest of confirmed hosts and the observed Swarm cluster ID for later scoped cleanup. There is no cleanup command or automatic registration of the new Swarm in this increment.

The Kubernetes preview produces k3s script templates only. It needs a separately reviewed installer file on every host; cluster execution, credential bootstrap, storage and recovery are not implemented. The [Kubernetes groundwork guide](16-kubernetes.md) covers only registration and workload rendering. No R9 gate is met.
