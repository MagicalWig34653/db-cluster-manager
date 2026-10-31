# E18 part 3: fresh-server bootstrap through the HTTP API and wizard: NOT RUN (2026-10-31)

Not executed. Reasons: the lab ran two long phases (part 1 and the two zero-UUID incidents, the second of which needs more than an hour because of the autoRecover cooldown), host load stayed at 12-30 and free disk fell to 25 GiB with the guard at 25 GiB, so a further VM would have been outside the lab rules (at least 40 GiB free before a run).

What a run needs (checked in code, not tried):

- Manager configuration `manager.serverBootstrap { enabled: true, agentRelease { version, binaryUrl (https), sha256 } }` plus `auth.enabled` and `manager.agentTransport`. The agent binary must be served over HTTPS with a certificate the fresh VM trusts (E17 used a lab-own CA installed in the VM).
- API: `POST /api/v1/bootstrap/drafts` (host, profile, write-only credential), `.../fingerprint`, `GET .../plan`, `.../approve`, `.../run`, optional `.../approve-lockdown`.
- Known gap from guide 27 that a real run would hit: the native-agent step asks for an enrollment token, which exists only for a host record that is already enrolled; a fresh server that is not yet a Swarm node has none ("enrollment token unavailable"). Join the Swarm first, sync and enroll, then run again; or fix the order in the product. Not verified.

Status stays as in E17: executor and SSH runner verified on Debian 12 and AlmaLinux 9 through a harness; API, wizard, native agent step, Swarm join, Ubuntu and Rocky not run on a real host.
