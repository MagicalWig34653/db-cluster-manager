# ADR 0002: Go backend and a single repository

Date: 2026-10-18
Status: Accepted for language and repository structure. Specific frameworks/protocols remain implementation proposals.

## Decision

Use Go for manager, host agent and CLI. Use one monorepo with separate frontend source. The intended frontend is SvelteKit/TypeScript with shadcn-svelte, built into static assets and distributed with the Go manager. Production does not require a separate Node.js frontend service.

Proposed layout: `cmd/manager/`, `cmd/agent/`, `cmd/cli/`, `internal/`, `web/`, `api/`, `deploy/`, `tests/`, `docs/` and `.github/workflows/`. Create directories as real implementation arrives.

## Rationale and alternatives

Go fits infrastructure integration and shared manager/agent protocols. Rust, C#/.NET, TypeScript/Node.js and Python were considered. The user explicitly selected Go after discussing development complexity, deployment and infrastructure ecosystem tradeoffs.

A monorepo permits coordinated API/UI/test/documentation changes and preserves durable project context. Separate packages/modules should maintain boundaries without requiring separate repositories.

## Consequences

Version API and agent contracts. Build frontend assets before manager packaging. Select toolchain/dependency versions during scaffold work; this ADR does not approve an untested library or storage system.

HTTP/JSON with OpenAPI, server-sent events and gRPC/mTLS are candidate communication choices. Replicated metadata storage, monitoring engine, proxy implementation and HA protocol require separate decisions.
