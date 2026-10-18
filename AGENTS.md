# Project instructions

## Read before work

Read `docs/STATUS.md`, `docs/WORKPLAN.md`, `docs/ENTSCHEIDUNGEN.md` and task-relevant requirements in `docs/KONZEPT.md`. Read `docs/TESTING.md` before any infrastructure work. The repository is the source of truth; sessions are disposable.

## Confirmed foundation

- Go for manager, agent and CLI. One monorepo; frontend code is separate, manager distribution includes the UI.
- SvelteKit/TypeScript with shadcn-svelte. No separate production Node.js server is intended.
- Swarm first, manager-deployed agent services, local storage, multi-Swarm support and replicated managers. Detailed requirements and release gates remain authoritative.
- English code, identifiers, comments and technical docs. German/English UI and user guides. Communicate with the user in German.
- AGPLv3; finalize the exact license declaration during bootstrap.

## Work and decisions

Proceed with authorized implementation and routine reversible choices. For a genuinely missing product decision, ask one focused question and wait; do not continue work that depends on its answer. Do not ask again whether to initialize Git or publish the repository: both are explicitly authorized for Claude's bootstrap.

Separate confirmed requirements, proposed mechanisms and verified implementation. Record architecture decisions with alternatives and evidence. Do not implement the entire roadmap in one step. Follow the current work package and release gates.

Keep requirements, contracts, docs and tests current with code. End each work package by updating `docs/STATUS.md` and `docs/WORKPLAN.md` with actual results, limitations and the next action. Do not depend on private memory or chat transcripts.

## Operational boundaries

Use dedicated local test environments only until the first working version is ready for the user's Swarm trial. Combined ceiling: 16 GB RAM, 50% host CPU capacity and approximately 100 GB SSD. Detect CPU capacity; do not infer core count from the chip name.

No global prune, host restart, Docker upgrade, unrelated resource modification, or deletion of unrelated Colima/Lima profiles. Use explicit contexts and an ownership inventory; provide scoped cleanup. Production credentials and backups must not enter tests or tracked files.

Automatic single-node replacement must not bypass quorum/recovery rules. Full-cluster recovery and major DB upgrades need explicit product-level approval. No hidden automatic promotion of cross-cluster backup copies.

## Commands currently available

- `python3 scripts/check_docs.py` — check local Markdown link targets and requirement coverage.

No Go module, frontend package or VM scripts exist yet. Add and document real commands when creating them; never report unexecuted checks as passing. GitHub workflows are prepared but not yet run remotely.

## Publication

Follow `docs/handoffs/CLAUDE-START.md`. Review the staged content before public publication; exclude secrets, machine-local settings, private paths and test data. Preserve configured Git identity and existing remotes; no force pushes or overwriting an unrelated repository. Use least-privilege CI permissions and immutable action pins when finalizing workflows.
