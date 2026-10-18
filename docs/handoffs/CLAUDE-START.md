# Claude Code bootstrap and development handover

## Authorized task

The user explicitly selected Go and the monorepo arrangement and asked Claude to initialize Git and push the project as a public GitHub repository. Publication does not require another generic permission question. This authorization concerns this project, not unrelated repositories or production infrastructure.

The local folder currently contains documentation, agent instructions and initial documentation checks/workflows. No application code, Git repository, remote or VM exists as a result of this preparation.

## 1. Read and inspect

Read `AGENTS.md`, `docs/STATUS.md`, `docs/WORKPLAN.md`, `docs/KONZEPT.md`, `docs/RELEASES.md`, `docs/ARCHITEKTUR.md` and `docs/TESTING.md`. Preserve all confirmed requirements.

Inspect available Git, GitHub CLI, Go, Node/package tooling, Python, Colima and Lima without changing unrelated setup. Check GitHub authentication and existing Git identity without exposing tokens. Verify whether the working directory has become a repository since handover.

## 2. Resolve publication coordinates

Default proposal: repository name `galera-cluster-manager`, under the clearly identified authenticated personal GitHub account, public, default branch `main`. The product name remains a working title. Use an explicitly configured project owner/name if one is already present.

If the target account is ambiguous, a conflicting repository exists, authentication is missing, or Git identity is absent, ask one concrete question for the missing information. Do not silently reuse or alter an unrelated remote/repository. Do not change global Git identity or invent a personal email. Prefer the user's existing intentional identity; discuss a GitHub noreply identity if needed.

AGPLv3 is confirmed, but the only-versus-or-later grant is not. Obtain that precise choice before adding the license declaration and first public push. Retrieve the complete corresponding AGPLv3 text from an authoritative source and verify it; do not substitute a summary. This is a missing licensing detail, not a request to reauthorize publication.

## 3. Prepare and publish

- Inspect the candidate file list and content; `.gitignore` is a starting filter, not proof that publication is clean.
- Finalize license, public README and relevant dependency/source notices. Do not copy the local UI-reference project's code automatically.
- Run `python3 scripts/check_docs.py`. Review workflows and pin third-party/GitHub actions to verified commit SHAs before first publication.
- Initialize Git on `main` if necessary, stage intended project files, inspect the staged diff and create the initial commit.
- Create the new public GitHub repository and push `main` using authenticated tooling. Do not force push. If a partial run already created this project's remote, inspect and resume safely.
- Record the repository URL and successful push in status. Verify remote visibility and commit rather than merely reporting an attempted command.
- Enable GitHub Pages with GitHub Actions as the source. Run/check the prepared Pages workflow, fix build or link issues and record the actual URL/result. Do not claim publication without a successful deployment.

GitHub CLI can perform creation with `gh repo create OWNER/NAME --public --source=. --remote=origin --push` after resolving the actual coordinates and creating a commit. Treat OWNER/NAME as placeholders, not literal values.

## 4. Continue with the first work package

Follow W02 onward in `docs/WORKPLAN.md`. Scaffold Go manager/agent/CLI and the separate Svelte frontend within the monorepo, keeping infrastructure mutation behind explicit operations. Confirm compatible dependency versions using primary documentation and lock them reproducibly.

API contracts, health/version endpoints and a built UI served by the manager are an appropriate first vertical slice. UI branding must state prototype status. Do not invent success data for unimplemented cluster operations.

Before real orchestration, resolve P0 decisions about manager state replication, coordination/fencing, Swarm service ownership and local-volume identity. Do not build custom consensus casually or treat three manager replicas as proof of HA.

## Completion and subsequent sessions

After each completed package: run relevant checks, update current status and workplan, commit coherent changes and keep the public remote current. Document failures and incomplete work precisely. Never upload secrets or raw lab data. Resume from repository state in a fresh session.
