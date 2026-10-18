# Current status

Updated: 2026-10-18.

## Phase

Concept documented; Claude Code handover prepared. Go and the monorepo are confirmed. The user explicitly authorized Claude to initialize Git and push a public GitHub repository.

No application code, Git repository, remote or test infrastructure has been created by this preparation. No production Swarm access has occurred.

## Completed

- 56 confirmed requirements, release allocation proposal and local test budget documented.
- Go/monorepo decision recorded in ADR 0002.
- `CLAUDE.md` imports shared `AGENTS.md` instructions.
- Bootstrap handover covers authentication, publication coordinates, licensing detail, initial commit/public push, Pages and subsequent development.
- Workplan W00–W06, `.gitignore`, a dependency-free documentation checker, documentation CI and Pages workflow prepared.

## Next action

Claude starts W01 via [the handover](handoffs/CLAUDE-START.md): inspect tools/authentication, resolve actual GitHub coordinates and the remaining AGPL-only/or-later choice, then initialize and publish. Public publication is already authorized; ask only for genuinely missing details. Pin verified action revisions before publication.

Continue with Go/Svelte scaffolding and P0 architecture/lab work. Replicated metadata, fencing, storage identity and several other implementation decisions remain open. The first working version must be validated locally before the user's Swarm trial.

## Verification and limits

`python3 scripts/check_docs.py` checks local Markdown file links and the mapping of 56 requirements to releases. It does not validate external URLs, anchors or rendered website links.

No application tests, VM tests, native architecture checks, Jekyll build, GitHub Actions run or publication has occurred. The Claude CLI itself has not been launched or configured globally. Workflow action tags are preparatory and must be pinned during bootstrap.
