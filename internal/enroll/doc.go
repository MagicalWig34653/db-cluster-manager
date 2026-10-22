// SPDX-License-Identifier: AGPL-3.0-only

// Package enroll implements agent enrollment and certificate renewal for
// the authenticated manager-agent transport (ADR 0010).
//
// Flow:
//
//  1. The manager creates an enrollment token (Token, NewToken) bound to the
//     installation, a set of allowed host IDs and an expiry, stores only its
//     SHA-256 digest, and deploys the plain token with the agent service as
//     a Swarm secret. The agent configuration also pins the SHA-256
//     fingerprint of the manager's internal CA certificate.
//  2. The agent generates its key locally (never sent anywhere) and posts a
//     CSR with the token and its host ID to the manager's enrollment
//     endpoint over TLS 1.3. It trusts the server only if the presented
//     chain contains a CA certificate with the pinned fingerprint, the
//     server certificate chains to it and carries the manager identity URI
//     SAN. There is no trust on first use.
//  3. The manager checks the token (digest, expiry, host allowed, not yet
//     used by this host: a replay is refused), optionally the request's
//     source address against the host's Swarm address, signs the CSR with
//     identities taken from its own records only, and records the issued
//     serial as the host's agent identity.
//  4. Renewal: the agent posts a new CSR over mTLS with its current
//     certificate. The manager accepts it only when that certificate's
//     serial is the one recorded for the host and is not revoked.
//  5. Revocation: the manager's deny list (internal/certs) rejects a serial
//     on every connection to an agent, in renewal, and (when distributed)
//     on the agent side.
//
// The package is transport and storage agnostic on the manager side
// (Authority, Registry); internal/manager implements them on the
// replicated store.
package enroll
