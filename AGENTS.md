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

- `make build` — build the frontend (`web/`, output embedded from `internal/webui/assets/`) and `bin/manager`, `bin/agent`, `bin/cli`, `bin/lab`.
- `make test` — Go tests and frontend (vitest) tests.
- `make check` — gofmt, `go vet`, `svelte-check` and the documentation check.
- `bin/manager -listen 127.0.0.1:8080` — serves `/api/v1/health`, `/api/v1/version` and the UI.
- `bin/lab status` — read-only host capacity, lab budget, ownership manifest (`.lab/manifest.json`, flag `-manifest`) and unrelated Lima/Colima inventory.
- `bin/lab plan [-vms N] [-cpus C] [-ram GiB] [-disk GiB]` — budget-checked dry run of the `dbcm-lab-mN` VM creation commands (defaults 3/2/4/20).
- `bin/lab apply [same flags] --apply` — creates the planned VMs with pre/post inventory snapshots; without `--apply` it is a dry run.
- `bin/lab cleanup` — preview of the manifest-scoped teardown; `bin/lab cleanup --apply` executes it and fails if an unrelated entry disappeared.

- `tests/p0/e1-swarm-galera.sh`, `tests/p0/e2-host-loss.sh` — recorded lab experiments; each creates and removes its own `dbcm-lab-` VMs (needs `bin/lab`).
- `bin/metad` — unauthenticated lab test harness for one metastore node (`cmd/metad/README.md`); never ship it as product API.

See `docs/TESTING.md` ("Lab commands"). `apply --apply` and `cleanup --apply` were run against the real host for the lab VMs only (W04, E1, E2). Add and document real commands when creating them; never report unexecuted checks as passing. SvelteKit 3 is configured through the `sveltekit(...)` Vite plugin in `web/vite.config.ts`; there is no `svelte.config.js`. GitHub workflows `docs-check`, `pages` ran successfully; `build` ran successfully.

## Publication

Follow `docs/handoffs/CLAUDE-START.md`. Review the staged content before public publication; exclude secrets, machine-local settings, private paths and test data. Preserve configured Git identity and existing remotes; no force pushes or overwriting an unrelated repository. Use least-privilege CI permissions and immutable action pins when finalizing workflows.
