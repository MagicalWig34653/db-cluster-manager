// SPDX-License-Identifier: AGPL-3.0-only

// Package runner is the manager-side operation executor. It turns an
// Operation (ordered steps, each an agent command or a gate) into fenced
// agent commands and records every step in the replicated operation journal
// (package opjournal, replicated by package metastore).
//
// Design sources: ADR 0003 (fencing by consensus term, unknown completion,
// barrier before a new leader trusts its journal) and ADR 0004 (local storage
// identity; destructive steps never run blindly).
//
// Rules enforced here:
//
//   - Only the metastore leader runs operations. Before it runs, resumes,
//     adopts, reconciles or aborts anything the runner checks IsLeader and
//     calls Barrier; a failed check sends no command. Immediately before every
//     agent command it checks again that it is leader and that the consensus
//     term is still the term that owns the operation. Every command carries
//     that term; agents reject older terms (package fencing).
//   - A step is journaled Running (StartStep) before its command is sent and
//     finished (FinishStep) only with an observed result. A transport error,
//     a timeout or an agent ErrPersist is Unknown, never Failed.
//   - Before an Unknown step is retried, the runner re-sends the same
//     (OpID, StepID). The agent returns its recorded result (Replayed) or
//     "in doubt". A recorded result is adopted. An in-doubt step of a
//     destructive kind pauses the operation (StatusPaused); only Reconcile,
//     after the operator or a reconciler has looked at the live state, can
//     continue it, and a retry then uses a new agent StepID. An unreachable
//     agent is queried a bounded number of times with backoff, then the
//     operation pauses. Only idempotent kinds (inspect, list-owned,
//     verify-identity, allow-objects) are retried automatically with a new
//     agent StepID, with bounded attempts and backoff.
//   - A new leader calls TakeOver: Barrier, then Adopt of every unfinished
//     journal operation with its own term (Running steps become Unknown),
//     then Resume applies the same rules. StartStep journals the agent
//     StepID of each attempt, so an Unknown step is queried under the ID
//     that was sent. A failure without effect whose retry is due (an
//     idempotent kind, or a destructive step's RetryCodes) is journaled
//     with FailForRetry; a new leader retries it under the next agent
//     StepID, counting the earlier attempts against MaxRetries, while a
//     plain Failed step stays failed.
//   - Abort happens only at a safe boundary: never while a step is Running.
//     An abort requested during a step takes effect before the next step.
//     An operator abort always ends Aborted; a step failure that it
//     coincides with is kept in the reason.
//   - Per-resource locks: two operations that name a common lock key (for
//     example "cluster/<id>") never execute at the same time. Locks are held
//     until the operation succeeds, is aborted, or this runner stops owning
//     it; a paused or failed operation keeps its locks.
//
// Limits (see also the package tests): operation definitions are kept in
// memory and must be supplied again to a new leader (TakeOver takes a lookup
// function); locks are per runner process, and a new leader derives the
// retry count of a due retry from the journaled attempts; per-attempt
// timeouts use real time (context.WithTimeout) while backoff and timestamps
// use the injected Sleep and Now. The agent transport behind AgentClient is
// not authenticated here.
package runner
