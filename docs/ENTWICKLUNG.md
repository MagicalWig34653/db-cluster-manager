# Development and durable project context

## Source of truth

Store requirements, decisions, work packages, contracts, implementation and verification evidence in the repository. Sessions are disposable; private agent memory must not be required to resume work.

New sessions read `README.md`, `docs/STATUS.md`, `docs/ENTSCHEIDUNGEN.md`, `docs/RELEASES.md` and task-relevant documents. Update the relevant canonical documents when behavior or decisions change, and record actual validation and next steps in status. Do not confuse planned features with implemented or verified behavior.

During brainstorming, ask one focused question and wait for the answer. The user explicitly requested this interaction style. Document confirmed answers without silently approving unresolved implementation choices.

## Languages and licensing

Code, identifiers and comments: English. Technical documentation: English initially. UI and user guides: German and English. Existing technical filenames are retained to keep links stable; their contents are now English.

The license is `AGPL-3.0-only` (confirmed by the user); the full text is in `LICENSE`. The copyright identity remains open. New source files should carry `SPDX-License-Identifier: AGPL-3.0-only`. Dependency licenses and any copied reference code require review.

## Claude Code handover

Project instructions are prepared in `CLAUDE.md` and `AGENTS.md`, with the executable documentation check, initial GitHub workflows, `docs/WORKPLAN.md` and the bootstrap in `docs/handoffs/CLAUDE-START.md`. Claude is explicitly authorized to initialize Git and publish a public repository. Application scaffolding, API contracts and verified local lab commands are the next implementation work; they do not exist yet.

Handover must identify what exists, what is proposed, actual checks, known limitations and the next concrete work package. Never invent commands or passing tests. Project instructions are context; critical ownership and permission invariants also require enforcement in code and tests.

## Definition of done for implementation work

- Acceptance criteria met and appropriate tests executed.
- User behavior/configuration/recovery documentation updated with the code.
- Compatibility and security-sensitive operational limitations documented.
- Durable status/task plan updated; no essential result lives only in chat.
- No secrets, private machine paths or customer data in tracked artifacts.

The release plan is canonical in [RELEASES.md](RELEASES.md); local verification constraints are in [TESTING.md](TESTING.md).

## GitHub Pages

The same `docs/` content is the repository and website source. A basic Jekyll configuration is prepared. English technical docs and German/English user guides will be published with clear navigation as guides are written.

Initial documentation-check and Pages workflows are prepared under `.github/workflows/`. Before publication, verify/pin action revisions. Still required: Git/GitHub repository setup, Pages source selection, actual site build/validation and publication. No GitHub target is configured and no page is published. Documentation checking belongs in CI once the development environment exists. GitHub Pages serves documentation, not the manager application.
