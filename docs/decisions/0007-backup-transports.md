# ADR 0007: S3 and SFTP transports for backup destinations

Date: 2026-10-22
Status: Proposed. Used by `internal/backup` (B01, B07). Reopen if a real S3 service or SFTP server shows incompatibilities, or if multi-part checksums or SSE features become requirements.

## Context

`backup.Backend` (Put/Get/List/Delete over opaque, already encrypted containers) had `ErrNotImplemented` stubs for S3 and SFTP. Both need a transport decision, and the S3 one needs a dependency decision.

## Decision

- **S3: hand-written SigV4 client on the standard library** (`s3.go`, `s3sign.go`). No new module.
- **SFTP: `github.com/pkg/sftp` v1.13.11 and `golang.org/x/crypto` v0.57.0** (`ssh`, `ssh/knownhosts`), both pinned exactly. `x/crypto` was already an indirect requirement at v0.57.0.

## Evidence

Collected with the Go module proxy in scratch modules under `$TMPDIR` (go1.27.1 darwin/arm64); licenses read from each module's `LICENSE` file.

| Option | Finding |
| --- | --- |
| `github.com/minio/minio-go/v7` v7.4.1 (2026-10-12) | Apache-2.0. Importing only the root package pulled a module graph of 304 edges and 231 modules in `go list -m all`. |
| `aws-sdk-go-v2/service/s3` v1.114.3 (2026-10-09) | Apache-2.0. 12 modules, 50 graph edges, 67 SDK/smithy packages in `go list -deps`. Needs config/credentials modules on top for a usable client. |
| `github.com/pkg/sftp` v1.13.11 (2026-07-12) | BSD-2-Clause (Dave Cheney). Scratch module: 20 graph edges. Adds `kr/fs` v0.1.0 (BSD-3-Clause) beyond what we already have. |
| `golang.org/x/crypto` v0.57.0 | BSD-3-Clause; already in `go.mod` (indirect, via webauthn). v0.58.0 exists; not taken to avoid an unrelated bump. |
| AGPL-3.0-only compatibility | Apache-2.0, BSD-2 and BSD-3 are all compatible with AGPLv3. Not legal advice, not counsel-checked. |

The S3 surface we need is small: five request kinds (PUT, multipart initiate/part/complete/abort, GET, HEAD, DELETE, ListObjectsV2). SigV4 is a documented, deterministic algorithm of roughly 100 lines, and it can be tested against published vectors. A 12-to-231-module dependency would cost more review and supply-chain surface than it saves. SSH and the SFTP protocol, by contrast, are security-critical protocol code that we should not rewrite, and `x/crypto/ssh` is already in the tree.

## Design

S3:
- Path-style and virtual-hosted addressing; custom endpoint (MinIO, Ceph); region; optional session token.
- TLS verification always on, optional extra CA bundle; there is no insecure switch. Minimum TLS 1.2.
- Every request signs `x-amz-content-sha256` (payload SHA-256). Put buffers at most one part (`PartSize`, default 16 MiB) in memory; larger streams use multipart upload and are aborted on any error or cancellation (abort uses a detached context with a timeout).
- Retries with exponential backoff and jitter for 5xx, 429 and transport errors (default 4 retries); 4xx is not retried; backoff honours the context.
- `Delete` does HEAD first because S3 DeleteObject succeeds for missing keys; this is not atomic with the delete.
- `List` uses ListObjectsV2 with continuation tokens, and only reports flat keys under the configured prefix.
- 404 on an object maps to `ErrNotFound`; a missing bucket and all other failures are `*S3Error`.

SFTP:
- Strict host key verification is mandatory: a `known_hosts` file (`knownhosts.New`) or a pinned `SHA256:` fingerprint. Configuring neither is an error. `InsecureIgnoreHostKey` is not used anywhere.
- Password and/or private key (optional passphrase) authentication.
- Upload to `.tmp-<random>` then `posix-rename@openssh.com`; servers without it fall back to remove+rename (not atomic, documented limitation). Failed or cancelled uploads remove the temp file.
- Names are validated by `validName` (no separators, no `..`, no `.tmp-` prefix), so no remote path leaves the configured directory.
- One lazily dialled SSH connection, reconnected once if it died; a semaphore bounds concurrent operations (default 4). A reader returned by `Get` holds a slot until closed.

## Verified and not verified

Verified (`go test -race -count=3 ./internal/backup/...`):
- SigV4 against the AWS `get-vanilla` test-suite vector and three worked examples from the S3 documentation, plus a step-by-step recomputation using only `crypto/hmac` and `crypto/sha256`.
- Client against an in-test fake S3 (`httptest`) that re-verifies every signature with its own, independently written canonicalisation; pagination, multipart (including exact multiples and abort), retries, error mapping, TLS and CA bundle, cancellation.
- SFTP against an in-process `x/crypto/ssh` server with `sftp.NewServer` on loopback: host key mismatch, known_hosts, auth failure, key auth, atomic rename, traversal rejection, reconnect, concurrency bound, cancellation.
- Both backends pass the shared `testBackend` suite.

Not verified:
- No real S3 service (AWS, MinIO, Ceph) and no real OpenSSH SFTP server were contacted. SigV4 is verified only against published vectors and our own fake; quirks of real services (for example XML escaping, `Expect: 100-continue`, eventual consistency, minimum 5 MiB part size, checksum requirements, object lock) are untested.
- Signature of requests with unusual keys is tested against the fake only.
- SFTP behaviour with servers lacking `posix-rename`, with chroot paths or with `known_hosts` hashed hosts/CA entries is untested beyond what `knownhosts` implements.

## Alternatives

minio-go and aws-sdk-go-v2 were rejected for dependency size as above; they remain a fallback if real-service testing exposes protocol corners (SSE, checksums, STS credential chains) that the hand-written client should not grow to cover.
