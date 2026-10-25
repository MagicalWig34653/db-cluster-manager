---
title: Configuration reference
---

# Configuration reference

[Guide index](index.md) | Previous: [Operator runbook](11-operator-runbook.md)

Derived from `cmd/manager/main.go`, `cmd/manager/managed.go` and `cmd/manager/peers.go` (state of 2026-10-23). The manager reads one JSON file, given with `-config <file>` or the environment variable `DCM_CONFIG`. **Unknown fields are rejected** and the manager does not start. Comments are not allowed in real JSON; the `//` notes below are for explanation only.

Verification: parsing and validation are unit-tested [Fakes]. The sections `auth`, `manager`, `manager.agentTransport`, `manager.backup` and `manager.settings` ran on lab VMs in the E4-E6 configuration (single instance as a process on a Swarm manager node) [Lab VMs]. `manager.keyring`, `manager.peerTLS`, several `peers`, `-print-replica-key` and `-issue-peer-certs` were only tested with fakes and loopback TLS [Fakes]; no multi-replica manager has run on VMs.

## Command line

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen addr` | `127.0.0.1:8080` | Address of the UI and API; native HTTPS with `manager.tls`, otherwise loopback HTTP behind a TLS proxy. |
| `-config file` | `$DCM_CONFIG` | JSON configuration. Without it the manager runs in prototype mode. |
| `-version` | | Print the version and exit. |
| `-healthcheck` | | Probe the loopback health endpoint at the `-listen` port; HTTPS with TLS config, HTTP otherwise; exit 0 if healthy. |
| `-print-replica-key` | | Print this replica's public data-key identity (`replica key` file in `manager.dataDir`, created if missing) and exit. Needs `-config` with `manager.dataDir`. |
| `-issue-peer-certs dir` | | Create a metastore peer CA and one certificate per replica in `dir`, then exit. The CA passphrase comes from the environment variable `DBCM_PEER_CA_PASSPHRASE` (at least 12 characters). Refuses to overwrite an existing CA. |
| `-installation id` | | Installation ID for `-issue-peer-certs` (required there). |
| `-peer-ids a,b,c` | | Comma-separated replica IDs for `-issue-peer-certs` (required there; letters, digits, `.`, `_`, `-`, at most 63 characters, starting with a letter or digit). |
| `-peer-validity d` | `8760h` (365 days) | Certificate validity for `-issue-peer-certs`. |

## Modes

| Configuration | Mode | State |
| --- | --- | --- |
| No file, or `auth.enabled` false and no `manager` | Prototype | Only `GET /api/v1/health`, `GET /api/v1/version` and the UI. |
| `auth.enabled` true, no `manager` | Auth mode | Accounts, sessions and audit **in memory**; resource endpoints answer 503. |
| `manager` section | Managed mode | Replicated metastore on disk; the leader serves the command API. Without `auth.enabled` there is no login and so no usable API. |

## `auth`

| Field | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `enabled` | bool | `false` | yes for managed mode | Turns on setup, passkey login, recovery and the full API. |
| `rpId` | string | | yes if enabled | WebAuthn relying-party ID, normally the DNS name of your HTTPS URL. |
| `rpDisplayName` | string | | recommended | Name shown by the passkey prompt. |
| `origins` | string list | | yes if enabled | Allowed origins: `https://` URLs whose host is `rpId` or a subdomain; `http://localhost[:port]` only for development. |

```json
{ "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "cluster-manager", "origins": ["https://db.example.com"] } }
```

## `manager`

Presence of this section selects managed mode. Required: `dataDir` and `bind`.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `nodeId` | string | `manager-1` | This replica's ID. Must match its entry in `peers` and the certificate identity when `peerTLS` is used. |
| `dataDir` | string | none, **required** | Metastore (`<dataDir>/metastore`), `data.key`, `replica.key` and the default monitoring store. Back it up, see the [runbook](11-operator-runbook.md). |
| `bind` | string | none, **required** | Metastore TCP address, for example `127.0.0.1:7946`. Without `peerTLS` the transport is plain TCP: loopback or isolated networks only. |
| `advertise` | string | empty | Address that peers dial, if it differs from `bind`. |
| `peers` | list of `{id, address}` | empty | Initial voting membership **including this replica**; empty means single-instance mode. There is no membership change afterwards. |
| `bootstrap` | bool | `false` | Write the initial membership (`peers`) on the first start of a new installation; ignored when the replica already has state on disk. All bootstrapping replicas must pass the same list, so set it to `true` on every replica of a new installation. |
| `keyFile` | string | `<dataDir>/data.key` | 32-byte data key for secrets at rest. Without `keyring` it is created if missing and must be copied to every replica by hand. With `keyring` an existing file is adopted as the first key when the keyring is bootstrapped; none is created. |
| `keyring` | bool | `false` | Replicated, wrapped data keyring ([ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md)). Each replica keeps only its identity in `<dataDir>/replica.key`. Recommended for more than one replica. |
| `peerTLS` | object | absent | Mutual TLS 1.3 between metastore replicas, see below. Recommended for more than one replica. |
| `labAgents` | bool | `false` | Unauthenticated lab HTTP agent endpoints. **Lab only**; excludes `agentTransport`. Without either, agent commands are refused. |
| `autoResume` | bool | `false` | A new leader resumes taken-over operations. |
| `agentTransport` | object | absent | Authenticated mTLS agent transport with enrollment ([ADR 0010](../../decisions/0010-agent-transport-and-enrollment.md)). Required in practice for hosts, clusters and backups. |
| `monitoring` | object | enabled | Metric store and alerts. |
| `backup` | object | absent | Manager side of cluster backups. |
| `imageBuild` | object | absent | Enables the image build service (proposal, fakes only), see below. Registry endpoints work without it. |
| `settings` | object | absent | Installation settings merged into the replicated settings on every leadership start (no API sets them yet). |

### `manager.peers[]`

| Field | Meaning |
| --- | --- |
| `id` | Replica ID, equal to that replica's `nodeId`. |
| `address` | Metastore address of that replica as dialed by the others. With `peerTLS` the replica answering at this address must present exactly this ID. |

### `manager.peerTLS`

All four fields are required if the section exists. Produced by `manager -issue-peer-certs`. The certificate file is re-read when it changes, so a reissued certificate needs no restart. Peers must be in `peers`; the certificate identity is `dbcm://<installation>/metastore/<replica ID>`.

| Field | Meaning |
| --- | --- |
| `installation` | Installation ID, the same on every replica and in `settings.installationId`. |
| `caFile` | Peer CA certificate (`ca.pem`). |
| `certFile` | This replica's chain (`<id>/peer.pem`). |
| `keyFile` | This replica's private key (`<id>/peer-key.pem`). |

### `manager.agentTransport`

`enrollListen` and `managerUrl` are required.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enrollListen` | string | none, **required** | TLS listen address of the enrollment endpoint, for example `0.0.0.0:7444`. |
| `managerUrl` | string | none, **required** | `https://host:port` under which agents reach `enrollListen`. An IP host is added to the certificate. |
| `serverDns` | string list | empty | Extra DNS names in the enrollment certificate. |
| `serverIps` | string list | empty | Extra IP addresses in the enrollment certificate (must be IPs). |
| `agentPort` | int | `7443` | Port on which agents listen on their hosts. |
| `allowAnySource` | bool | `false` | Disables the check that an enrollment request comes from the claimed host's Swarm address. Needed behind NAT only; weakens enrollment. |
| `monitorUser` | string | empty | Database monitoring account the agents use for health collection. Needed for health collection and the full-outage notification. |
| `monitorSecret` | string | empty | Name of an existing Swarm secret with that account's password. |
| `dbSecrets` | string list | empty | Further existing Swarm secrets mounted into the agent service (cluster root and backup account passwords). Applied only when the agent service is created; the secrets need the label `dbcm.installation=<installation ID>`. |

### `manager.monitoring`

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `disabled` | bool | `false` | Turns off metric collection, alerting and metric queries; the notification settings keep working. |
| `dataDir` | string | `<manager.dataDir>/monitoring` | Metric store; must be an absolute path. |

### `manager.backup`

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `backendConfigFile` | string | empty | Absolute path of a JSON file with the same content as the jobs' backend secret: `{"type": "sftp" or "s3" or "dir", ...}` plus an `sftp`, `s3` or `dir` section (host key verification is mandatory for SFTP). Without it backup conditions stay unmet and restores are refused (`settings_incomplete`). `dir` works for lab tests only. |
| `scheduleIntervalSeconds` | int | `60` | How often the leader evaluates schedules, retention and inspection expiry; minimum 10. |

### `manager.imageBuild`

Presence enables image builds (`POST /image-builds`); without it the build routes answer 503 while `/registries` still works. The manager runs `docker buildx build` as its own process. The shipped manager image has no docker CLI and the stack gives it no BuildKit, so a build needs both added first [Untested]. `manager init -image-build` writes this section with its defaults.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `dockerBinary` | string | `docker` | Docker CLI path or command name (no arguments). |
| `workDir` | string | OS temp dir | Absolute directory for temporary build contexts and docker configs (the latter hold the registry credential, mode 0600, removed after the build). |
| `maxLogBytes` | int | `262144` | Cap of the stored, redacted job log; minimum 1024. |

Registry credentials, job records and plans are kept in the replicated store; credentials are sealed with the data key (`keyFile` or `keyring`) like session digests.

### `manager.settings`

Merged by the leader at every leadership start; empty fields leave stored values alone. `installationId` cannot change once set; `volumeBase` cannot change while a cluster exists.

| Field | Type | Meaning |
| --- | --- | --- |
| `installationId` | string | Identifies objects owned by this installation (label `dbcm.installation`). **Required before any cluster can be created.** |
| `volumeBase` | string | Clean absolute directory on every host under which node volumes live. **Required before any cluster can be created.** Must exist on every host. |
| `agentImage` | string | Image of the agent service. **Required to deploy agents.** |
| `monitorSocket` | string | Relative path (below the volume root) of the node's monitoring socket. |
| `accounts` | object | Database accounts of new clusters: `monitorUser`, `monitorSecret`, `backupUser`, `backupSecret` (names of Swarm secrets) and `backupLogical` (bool, also grants the read privileges `mariadb-dump` needs). |
| `backup` | object | Backup settings, see below. |

`settings.backup` (all required before a backup can start): `jobImage` (image with `dbcm-job` and the backup tools), `backendSecret` (Swarm secret with the backend JSON), `dbCredentialSecret` (backup account credential), `recipientKey` (public half of the recovery key; backups are encrypted for it, and the same key is the recovery envelope of the data keyring). Further: `keySecret` (default Swarm secret with the recovery key for restores), `inspectionNetwork` (non-production network for inspection restores; required for them).

## Vulnerability response

**[Fakes] only:** wired into the manager and tested with fakes and `httptest`; no real feed or AI provider was contacted. The section `manager.vuln` enables it (needs `auth.enabled`; absent: routes answer 503, nothing runs). The policy, providers and consent are stored in the metastore and changed in the UI or through the API (permission on resource `vuln`, administrator preset only).

`manager.vuln`:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `pollIntervalSeconds` | int | `3600` | Feed polling interval of the leader, at least 300. The first poll runs 30 s after leadership starts. |
| `feeds` | list | empty | Advisory sources, each kind once: `{"kind": "osv" | "github" | "nvd" | "kev", "url": "https://..."}`, optionally `apiKeyFile` (absolute path of a file holding a token or API key; `github` and `nvd` only), or `{"kind": "osvFile", "path": "/abs/mirror.json"}` for an offline OSV mirror. There are no default URLs; `http` is accepted only for loopback hosts. No feed listed: nothing is fetched. |
| `defaultMode` | string | `notify_only` | Seeds the policy once, when none is stored: `notify_only` or `approval_required`. Never overwrites a stored policy; `auto_for_policy` can only be set through the UI or API. |
| `providers` | list | empty | AI providers registered when their ID does not exist yet: `id`, `name`, `kind` (`claude_api`, `openai_api` or `openai_compatible`; CLI kinds only through the API), `baseUrl`, `models`, `defaultModel`, `apiKeyFile` (read once and sealed into the metastore), `limits`, `maxOutputTokens`, `timeoutSeconds`. They stay unusable until consent is granted in the UI. |

Feed and provider requests follow no redirects. Notifications (new finding, mitigation expiring) go through the notification dispatcher as events of kind `vuln.*` and follow your routing rules.

### API settings

Policy (`PUT /api/v1/vuln/policy`, conditional on `version`):

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `mode` | string | `notify_only` | `notify_only`, `approval_required` or `auto_for_policy`. |
| `provider`, `model` | string | empty | AI provider ID and model for analyses; both or neither. Empty means deterministic proposals only. |
| `auto.severities` | list | empty | Severities that may be auto-approved (`auto_for_policy` only, required). |
| `auto.services` | list | empty | Named services that may be auto-approved; no wildcard (required). |
| `auto.kinds` | list | empty | `patched_rebuild` and/or `mitigation` (required). |
| `auto.maxPerDay` | int | none | Automatic approvals per UTC day, 1-20 (required). |
| `killSwitch` | bool | `false` | Stops all automatic approvals. |
| `mitigationDays` | int | `30` | Lifetime of a mitigation, 1-90 days. |
| `retainPrompts` | bool | `false` | Reserved; prompts are not retained yet (only their hashes). |

AI provider (`POST /api/v1/vuln/providers`):

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `id`, `name` | string | none | Identifier and display name. |
| `kind` | string | none | `claude_api`, `openai_api`, `openai_compatible`, `claude_code_cli`, `codex_cli`. |
| `baseUrl` | string | Claude: `https://api.anthropic.com`; OpenAI: `https://api.openai.com/v1` | Required for `openai_compatible` (for example `http://127.0.0.1:1234/v1`). Plain http only to loopback or private addresses. |
| `models`, `defaultModel` | list, string | empty | Selectable models; empty allows any well-formed name. |
| `apiKey` | string | empty | Write-only; required for `claude_api` and `openai_api`, optional for `openai_compatible`, refused for CLI kinds. Stored sealed. |
| `binary`, `home` | string | empty | CLI kinds only: absolute path of the CLI and of the directory holding its login state. |
| `experimental` | bool | `false` | Must be `true` for CLI kinds. |
| `limits.requestsPerDay`, `limits.tokensPerDay` | int | `50`, `200000` | Usage limits per UTC day. |
| `maxOutputTokens` | int | `2048` | Output token cap, at most 16384. |
| `timeoutSeconds` | int | `120` | Request or CLI timeout, at most 600. |

Consent: `PUT /api/v1/vuln/providers/{id}/consent` with `{"grant": true, "statement": <egressStatement>}`; revoke with `{"grant": false}`.

Feeds are configured in `manager.vuln.feeds` (above); none has a default URL.

## What managed mode needs

| Goal | Required configuration |
| --- | --- |
| Start managed mode | `auth.enabled` with `rpId` and `origins`; `manager.dataDir`; `manager.bind`; `manager.bootstrap: true` for the first start. |
| Enroll hosts, deploy agents | `manager.agentTransport` (`enrollListen`, `managerUrl`); `settings.installationId`, `settings.agentImage`, `settings.volumeBase`. |
| Health, outage events | `agentTransport.monitorUser` and `monitorSecret`; `settings.accounts`. |
| Create clusters | All of the above plus `accounts`; networks and secrets labelled with the installation ID. |
| Backups, restores | `manager.backup.backendConfigFile`; `settings.backup` (job image, backend secret, credential secret, recipient key, inspection network, key secret). |
| Several replicas | `peers` on every replica, `keyring`, `peerTLS`; see the runbook. A multi-replica run on VMs has not happened [Untested]. |

## Example: single instance

```json
{
  "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "cluster-manager", "origins": ["https://db.example.com"] },
  "manager": {
    "nodeId": "manager-1",
    "dataDir": "/var/lib/dbcm-manager",
    "bind": "127.0.0.1:7946",
    "bootstrap": true,
    "autoResume": true,
    "agentTransport": {
      "enrollListen": "0.0.0.0:7444",
      "managerUrl": "https://10.0.0.4:7444",
      "monitorUser": "dbcm_monitor",
      "monitorSecret": "dbcm-inst-monitor-pw"
    },
    "backup": { "backendConfigFile": "/etc/dbcm/backend.json" },
    "settings": {
      "installationId": "inst-1",
      "volumeBase": "/var/lib/dbcm",
      "agentImage": "registry.example.com/dbcm-agent:0.0.0-dev",
      "accounts": { "monitorUser": "dbcm_monitor", "monitorSecret": "dbcm-inst-monitor-pw",
                    "backupUser": "dbcm_backup", "backupSecret": "dbcm-inst-backup-pw", "backupLogical": true },
      "backup": { "jobImage": "registry.example.com/dbcm-jobs:0.0.0-dev", "backendSecret": "dbcm-inst-backup-backend",
                  "dbCredentialSecret": "dbcm-inst-backup-pw", "recipientKey": "<public recovery key>",
                  "keySecret": "dbcm-inst-recovery-key", "inspectionNetwork": "dbcm-inspect" }
    }
  }
}
```

All names above are examples. The secret and network names must exist in the Swarm and carry the label `dbcm.installation=inst-1`.

## Example: replica fragment with keyring and peer TLS

For replica `manager-2` of three, after `manager -issue-peer-certs ./peers -installation inst-1 -peer-ids manager-1,manager-2,manager-3` and copying `./peers/manager-2/` to that host (the sealed CA `ca.sealed` and the passphrase stay offline):

```json
"manager": {
  "nodeId": "manager-2",
  "dataDir": "/var/lib/dbcm-manager",
  "bind": "0.0.0.0:7946",
  "advertise": "10.0.0.5:7946",
  "peers": [ {"id": "manager-1", "address": "10.0.0.4:7946"},
             {"id": "manager-2", "address": "10.0.0.5:7946"},
             {"id": "manager-3", "address": "10.0.0.6:7946"} ],
  "bootstrap": true,
  "keyring": true,
  "peerTLS": { "installation": "inst-1", "caFile": "/etc/dbcm/peer/ca.pem",
               "certFile": "/etc/dbcm/peer/peer.pem", "keyFile": "/etc/dbcm/peer/peer-key.pem" }
}
```

The registration procedure for the keyring is in the [operator runbook](11-operator-runbook.md#5-replicas-key-ring-and-peer-certificates). Every replica also needs a local Swarm manager Docker socket (see [Known limits](10-known-limits.md)).

### `manager.tls` and `manager.tls.acme`

Implemented for the API/UI listener [Fakes]; unrelated to `peerTLS` or `agentTransport`. See [Manager HTTPS and ACME](17-manager-tls.md) for setup and HA requirements. By default plain HTTP is restricted to numeric loopback addresses; exposed HTTP requires an explicit opt-in. The container's prototype default now binds loopback too.

| Field | Default | Meaning |
| --- | --- | --- |
| `tls.certFile`, `tls.keyFile` | absent | Imported PEM certificate chain/private key; both required together; loaded at startup. Excludes ACME. |
| `tls.allowPlainHttp` | `false` | Explicit exposed-HTTP permission, for a protected listener behind your TLS proxy. Excludes certificate settings. |
| `tls.acme` | absent | Enable automatic HTTPS and accept this CA's terms; requires managed mode. |
| `tls.acme.directoryUrl` | `https://acme-staging-v02.api.letsencrypt.org/directory` | HTTPS RFC 8555 directory. Set it explicitly; production requires an explicit production URL. |
| `tls.acme.email` | required | Plain account contact address. |
| `tls.acme.domains` | required | Nonempty, unique lowercase ASCII/punycode DNS names; no wildcards or IPs. Identical on all replicas. |
| `tls.acme.challenge` | `tls-alpn-01` | `tls-alpn-01` (public 443 to the TLS listener) or `http-01` (public 80 to the challenge listener). |
| `tls.acme.httpListen` | `:80` | Separate challenge-only HTTP bind, used only for HTTP-01. Use an unprivileged port with external forwarding in the shipped container. |

The API/UI port remains `-listen`; configuring ACME does not change its default from 8080. Renewals run on the leader (on election and hourly, within 30 days of expiry; five-minute retry after failure). Certificates, account keys and challenges are sealed in the replicated metastore. All replicas need accessible data keys. `-healthcheck` reads the same config and uses verified HTTPS with the first ACME domain as SNI; static TLS uses `auth.rpId` as SNI (otherwise the loopback address). Staging/private CAs need an installed trust root (`SSL_CERT_FILE` is supported by Go); no verification bypass exists.
