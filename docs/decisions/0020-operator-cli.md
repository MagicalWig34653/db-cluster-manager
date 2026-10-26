# ADR 0020: Operator CLI over the existing REST API

Date: 2026-10-26
Status: Implemented for the current API, tested with `httptest.ResponseRecorder`; socket-backed tests require an environment that permits local listeners.

## Context

`cmd/cli` previously exposed only `version`. The operator needs contexts, readable listings, request-file based mutations, plan export, approvals and automation-friendly exit codes. The manager authenticates operator API routes with a session cookie. Only proxy endpoints have Bearer tokens; no general operator API token issuance endpoint exists.

## Decision

The CLI uses the existing `/api/v1` routes, sends the configured Origin header for mutations and one opaque session cookie. It accepts a session token only from stdin, environment or file, validates it against `/auth/session`, and stores it in a separate 0600 file per context. The context file contains endpoints and file paths, never a token. HTTPS is required except for localhost development. Redirects are not followed, so a token cannot be forwarded to a redirected host.

Request bodies for cluster and service creation or planning come from JSON files. The CLI shows the server's stored plan and requires a separate explicit digest approval. `backups verify CLUSTER` only checks the last recorded passed restore verification; with `--result-file`, it starts the API's isolated inspection restore and stores the one-time password in a private file. Approval and operation completion remain separate. A service plan can be exported as JSON or YAML through the API; cluster creation has no preview route.

## Alternatives and limits

- Add generic Bearer token support to the CLI without a manager endpoint: rejected because it would promise a login mode the manager cannot authenticate. A future token issuance/authentication design needs a separate API and threat-model decision.
- Store the session token inside the context file: rejected to keep endpoint configuration shareable without copying credentials.
- Treat a backup run as verified: rejected because backup completion is not restore verification. The inspection restore and its verification timestamp are the current API evidence.
- Infer an approval digest from a new plan during approval: rejected; the operator must pass the digest they reviewed.

This decision adds no new API route or authorization rule. It does not make WebAuthn/passkey login possible in a terminal, nor does it test a real manager. The CLI must be exercised in an environment permitting local listeners and then against a manager before claiming end-to-end readiness.
