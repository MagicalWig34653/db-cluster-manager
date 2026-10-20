// SPDX-License-Identifier: AGPL-3.0-only

// Package agent implements the core of the per-host agent: the volume
// identity marker, a fenced and idempotent command executor, and the
// ownership guard that limits which Docker objects the agent may touch.
//
// Design sources: docs/ARCHITEKTUR.md, ADR 0003 (fencing by consensus term)
// and ADR 0004 (ownership labels, pinned services, local-storage identity).
// The identity check is a Go port of tests/p0/marker-guard.sh, whose
// semantics were exercised in lab experiment E2.
//
// Safety rules enforced here:
//
//   - Every command passes fencing.Guard.Admit(term) before any effect.
//     Commands from a stale term are rejected without effect.
//   - Commands are idempotent per (OpID, StepID). The outcome of each step is
//     recorded in a durable JSON state file. A re-sent step returns the
//     recorded outcome instead of executing again. A step whose execution
//     started but whose outcome was never recorded (crash) is reported as
//     StatusUnknown and is never executed again under the same IDs; the
//     manager must reconcile live state and issue a new StepID.
//   - The executor touches only Docker objects whose ID is in its allow-list
//     AND whose dbcm.installation label equals the configured installation.
//     Labels alone are bookkeeping, not a security boundary.
//   - Destructive or start operations on database volumes verify the identity
//     marker first. An empty volume is accepted only through a single-use
//     "new identity" token issued by the manager.
//
// Transport: Handle is transport-agnostic. The HTTP/JSON adapter in
// http.go is an UNAUTHENTICATED LAB TRANSPORT. It only listens on loopback
// addresses and rejects non-loopback peers, and it must not be used in
// production. Authenticated manager-agent transport (outbound agent
// connections with mTLS, enrollment, rotation and revocation) is future work.
//
// The Docker Engine client lives in internal/dockerclient. Not implemented
// here: reconciliation of in-doubt steps and compaction of the step record.
package agent
