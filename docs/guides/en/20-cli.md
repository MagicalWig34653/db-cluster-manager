---
title: Operator CLI
---

# cli(1) — operator command line

**[Fakes]** The Go CLI uses the existing `/api/v1` REST API. Its command dispatch has unit tests and `httptest` server tests; no real manager login or cluster operation has been exercised through this CLI. The API contract is [OpenAPI](../../../api/openapi.yaml). See the [German guide](../de/20-cli.md).

## Synopsis

```text
cli [--config PATH] [--context NAME] [--output table|json] COMMAND
cli help
```

The binary is `bin/cli` after `make build`. Global options precede the command. Table is the default; `--output json` prints the API response with indentation. IDs and digests are ordinary arguments. **Never supply a secret as an argument.**

## Configuration and login

```sh
cli contexts set lab https://manager.example https://manager.example
cli contexts use lab
CLI_SESSION_TOKEN_FILE=/private/path/session.token cli login
cli whoami
cli contexts list
cli logout
```

The context file defaults to the OS user config directory at `cluster-manager/cli.json`; override it with `CLI_CONFIG` or `--config`. It contains `currentContext`, `contexts`, and each context's `url`, `origin`, and optional `sessionFile`. `contexts set NAME URL ORIGIN` writes it with mode 0600. The URL must use HTTPS; plain HTTP is accepted only for `localhost`. `origin` must match the URL origin because manager state changes require its configured Origin header. `--context NAME` selects another context for one invocation. Local logout removes the stored token; it does not revoke the server session. Server expiry still applies.

`cli login --token-stdin`, `cli login --token-file PATH`, `CLI_SESSION_TOKEN`, or `CLI_SESSION_TOKEN_FILE` accepts an **existing opaque manager session token**. Login validates it with `GET /auth/session` and stores it in a context-specific 0600 file. The environment variables can also supply the token to any command without saving it. The manager currently issues operator sessions through passkey login in the web UI and accepts only its session cookie for operator routes. It has **no general API-token issuance or Bearer-token login endpoint**; proxy writer Bearer tokens are unrelated and will not work here. Obtain a session token through the existing passkey flow and transfer it through a protected input channel. Do not put it in shell history, tracked files, or a URL. Session files are sensitive; protect the containing directory too.

## Commands

| Command | API action and result |
| --- | --- |
| `clusters list`, `clusters get ID` | List or get visible clusters. |
| `clusters create --file JSON` | `POST /clusters`; this plans and starts creation immediately. There is no separate create-preview route. |
| `clusters plan ID --file JSON` | `POST /clusters/{id}/operation-plan`; stores a digest-bound Galera operation. |
| `clusters delete ID` | `DELETE /clusters/{id}` creates a deletion plan; it does not remove the cluster until approved. |
| `clusters approve OPERATION DIGEST` | Approve the displayed operation definition digest. Alias for `operations approve`. |
| `operations list`, `operations watch ID [--interval SECONDS]` | List or poll until succeeded, failed, aborted, or unknown. Watch defaults to two seconds. |
| `operations abort ID`, `operations approve ID DIGEST` | Request a safe-boundary abort or send the displayed digest. |
| `backups list`, `backups run CLUSTER [--file JSON]` | Show backup summaries or start a backup. The optional JSON file selects `method` and `databases`; default is physical. |
| `backups verify CLUSTER` | Read the last *passed restore verification* timestamp; exit 7 if none is recorded. This does not run a restore. |
| `backups verify CLUSTER --result-file PATH [--backup ID] [--host ID]` | Start an isolated inspection restore, writing the full response, including its one-time inspection password, to a 0600 result file. The displayed summary omits the password. Review and approve its operation digest separately; success is only established when the operation completes and the verification timestamp appears. |
| `hosts list` | List visible Swarm hosts. |
| `services plan --file JSON`, `services create --file JSON` | Preview or request a service. Service creation may wait for operation approval. |
| `vuln findings list`, `vuln approve PROPOSAL DIGEST` | List findings or approve a remediation proposal against its displayed digest. Availability depends on the manager's vulnerability configuration and policy. |
| `plan export --file JSON [--format json\|yaml]` | Export a service plan from `POST /services/plan`; redirects nothing to disk unless the caller uses shell redirection. |
| `completion bash\|zsh\|fish` | Print a shell completion script. |

Input files are request bodies matching OpenAPI. For example, use `cli --output json clusters delete c1` to capture its operation ID and `approval.digest`, review the plan, then `cli clusters approve OPERATION DIGEST`. A 409 means the displayed digest or underlying state is stale; fetch a fresh plan. A plan export is a service preview; cluster creation has no standalone preview endpoint. Full-cluster recovery and major upgrades keep their separate product-level approval boundaries.

## Exit status

| Code | Meaning |
| --- | --- |
| 0 | Command succeeded; `watch` reached `succeeded`. |
| 1 | Other API/response error, or watched operation failed/aborted/unknown. |
| 2 | Usage, input, or local configuration error. |
| 3 | Unauthenticated or forbidden, including missing local session. |
| 4 | API resource or backup summary missing. |
| 5 | Conflict, including approval mismatch. |
| 6 | API unavailable or network error. |
| 7 | No passed restore verification recorded. |

HTTP status and coarse API error code go to stderr; the CLI does not print server error bodies. The server remains authoritative for permissions, audit, approval and safety gates.
