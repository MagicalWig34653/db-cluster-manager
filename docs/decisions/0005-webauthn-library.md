# ADR 0005: WebAuthn verification library for passkeys

Date: 2026-10-20
Status: Proposed. Used by `internal/passkey` (U01). Reopen if the evidence below changes or a security review rejects it.

## Context

U01 makes passkeys the primary credential and allows passkey-only accounts. The manager has to verify WebAuthn registration (attestation) and authentication (assertion) responses. [ARCHITEKTUR](../ARCHITEKTUR.md) adds two constraints: passkeys need a secure browser origin, and the relying-party (RP) identity must stay stable across recovery and address changes.

The question here is narrow: verify the WebAuthn responses ourselves, or use `github.com/go-webauthn/webauthn`? Either way the project's own API stays the same (`internal/passkey`: `BeginRegistration`, `FinishRegistration`, `BeginLogin`, `FinishLogin`), and so do challenge storage, RP configuration validation and the account link.

## Decision

Use **`github.com/go-webauthn/webauthn` v0.18.2**, pinned exactly in `go.mod`, only from inside `internal/passkey`. No library type appears in the package's exported API, so the library can be swapped without changing callers.

## Evidence

I used primary sources only. The module proxy gave me `go list -m -versions` and `go list -m -json` (versions and publish times). I also read the `LICENSE`, `README.md`, `SECURITY.md` and source files of the downloaded module, ran `go doc`, and ran `go mod graph` and `go list -deps` in a scratch module under `$TMPDIR`. Toolchain: go1.27.1 darwin/arm64.

| Item | Finding |
| --- | --- |
| Latest version | v0.18.2, published 2026-09-19 (`go list -m -json ...@latest`) |
| Release cadence | 64 tagged versions. From v0.15.0 (2025-11-09) to v0.18.2 there are 15 releases: v0.16.0 2026-03-01, v0.17.0 2026-04-21, v0.17.4 2026-05-22, v0.18.0 2026-08-27, v0.18.1 2026-09-10, v0.18.2 2026-09-19 |
| License | `LICENSE` is BSD 3-Clause ("Redistribution and use ... Neither the name of the copyright holder ..."), copyright "github.com/go-webauthn/webauthn authors" |
| AGPL-3.0-only compatibility | BSD-3-Clause is GPL-compatible, so it can be combined with AGPL-3.0-only. The notice must be kept in distributions. Not legal advice and not checked by counsel |
| Transitive licenses (each module's `LICENSE` file) | MIT: fxamacker/cbor, go-viper/mapstructure, golang-jwt/jwt, philhofer/fwd, tinylib/msgp, x448/float16. BSD-3-Clause: go-webauthn/x, google/uuid, golang.org/x/crypto. Apache-2.0: google/go-tpm. All compatible with AGPL-3.0-only (Apache-2.0 with GPLv3-family licenses) |
| Module `go.mod` | `go 1.26.0`, `toolchain go1.27.1`, 9 direct requires (includes test-only testify and go.uber.org/mock) |
| `go mod graph` (scratch module) | 49 edges |
| External modules in the build of `internal/passkey` (`go list -deps`) | 12: go-webauthn/webauthn, go-webauthn/x, fxamacker/cbor/v2, x448/float16, go-viper/mapstructure/v2, golang-jwt/jwt/v5, google/go-tpm, google/uuid, tinylib/msgp, philhofer/fwd, golang.org/x/crypto, golang.org/x/sys (x/sys was already required) |
| Size | about 16,000 non-test lines in `protocol/` and `webauthn/` (including generated MessagePack code) |
| Stability | README "Status": still version 0; "there may be breaking changes without warning" |
| Security process | `SECURITY.md`: coordinated disclosure via GitHub security advisories |
| Features we need (`go doc`, source) | `BeginRegistration`/`CreateCredential`, `BeginLogin`/`ValidateLogin`, `BeginDiscoverableLogin`/`ValidatePasskeyLogin`; parsing from bytes (`protocol.ParseCredentialCreationResponseBytes`, `ParseCredentialRequestResponseBytes`), so no `net/http` coupling; UV required via session; RP ID hash, origin, challenge, ceremony type and signature checks; attestation formats including `none` and `packed`; sign-counter comparison exposed as `Authenticator.CloneWarning`; backup-flag consistency checks |
| Our integration result | `go test -race -count=3 ./internal/passkey/` passes, with real ES256 ceremonies from a software authenticator written in the test (no mocks of verification). That covers `none` and `packed` self attestation and the negative cases listed in the package tests |

## Alternatives

**Own verification.** We would have to write CBOR decoding (attestation object, COSE keys), authenticatorData parsing, clientDataJSON checks, origin and RP ID hash checks, ECDSA/RSA/EdDSA signature verification over COSE keys, and at least the `none` and `packed` attestation formats, with self and x5c variants. All of it would be new security-critical code. Its only advantage is fewer dependencies (CBOR alone is about 1 module). Rejected: the attack surface of hand-written parsers is worse than a maintained library's, and nothing in the project needs control at that level.

**`go-webauthn/webauthn` (chosen).** Cost: 11 new external modules. These include JWT and TPM libraries that we do not need (they serve attestation formats and metadata). Also a v0 API that may break, which is why the version is pinned exactly and the library stays behind our own interface.

Other Go WebAuthn libraries were not evaluated.

## Consequences and project-side policy

`internal/passkey` owns these policies; the library is configured to match:

- RP configuration is validated before the library sees it. Origins must be https, except `http://localhost[:port]`. The RP ID must equal the origin host or be a suffix of it at a label boundary. IP-address RP IDs are rejected. Origins are normalized (lower case, default port dropped).
- The RP ID has to be chosen once and kept. A changed RP ID orphans every passkey. Credentials store the RP ID they were registered under, and credentials from another RP ID are never offered for login.
- User verification is required, and the UV flag is checked explicitly in addition to the library's check. Resident keys are required, so passkey-only accounts and user-name-less login work. Attestation conveyance is `none`. Attestation statements that still arrive are verified but never trusted: there is no metadata service and no attestation root.
- Challenge expiry uses the injected clock in our challenge store. The library's own wall-clock enforcement (`Timeouts.*.Enforce`) stays off. Ceremonies are single-use: consumed on the first finish attempt, whatever the outcome. They are bound to a caller-supplied session binding, and for registration and named login also to the account.
- Sign counter: the library's `CloneWarning` (counter not increasing while either value is non-zero) rejects the assertion. The stored counter is left unchanged, and the event is recorded on the credential for audit and notification. Counters that stay zero, as with synced passkeys, are accepted. Whether to also suspend the credential is a product decision that is still open.
- The account store (`internal/recovery`) decides which passkeys an account has. A passkey that recovery removed cannot log in, even while its key material is still in the passkey store.

## Not verified

- The README's claim that the library is "conformance tested against the conformance tools". I did not run the FIDO conformance suite.
- Real browsers and hardware or platform authenticators: none were tested. All ceremonies ran against the test's software authenticator.
- Attestation formats other than `none` and `packed` self attestation (no x5c chain, TPM, Android, Apple or FIDO U2F).
- Algorithms other than ES256.
- The library's vulnerability history. `govulncheck` is not installed here, and I did not read GitHub security advisories.
- Whether the library can be configured to refuse to parse unused attestation formats. If it cannot, the JWT and TPM code stays reachable from attacker-supplied registration data.
- Public-suffix checks for the RP ID. Browsers enforce them; we do not.
- Legal review of license compatibility.
