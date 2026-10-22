# UI and user experience

## Reference and language

UI technology: Svelte and shadcn-svelte. The local `timetrack-pwa` reference was inspected through `package.json`, `components.json`, `src/routes/layout.css` and the application layout. No browser visual inspection was performed.

Observed visual direction: neutral Zinc surfaces, Inter typography, fine borders, rounded panels, light/dark modes, configurable accent colors, fixed desktop sidebar, mobile drawer, top bar and restrained translucent framing with accessible fallbacks. Treat these as visual inspiration, not an automatic source-code or license transfer.

UI supports German/English per user. Code and comments are English. User guides are bilingual. This project must remain understandable without access to the local reference repository.

## Navigation proposal

Overview · Swarms · Clusters · Hosts · Databases · Backups · Dashboards · Operations · Events · Settings.

Show cluster health, data freshness, endpoint, node roles, last verified restore and automation policy directly. Unknown observations must not look healthy. Explain blocking conditions and offer relevant next actions.

### Navigation additions for the broadened scope (proposal)

Proposed additions, not implemented ([ADR 0012](decisions/0012-broadened-scope-platform.md)): **Services** (all managed service types: web servers, storage servers, generic containers, Traefik, registry), **Images & Registry** (build recipes, builds, SBOM/scan results, registries and distribution rules) and **Platforms** (Swarm and later Kubernetes environments, cluster provisioning, Docker engine updates). Galera clusters keep their own primary entry (Clusters) and remain the first-class flow; the new entries appear only when the corresponding features exist.

## First run

1. Choose new installation or restore manager configuration.
2. For recovery: upload backup or connect S3/SFTP, choose backup and supply recovery key.
3. For a new installation: establish administrator/passkey and recovery method, then connect a Swarm.
4. Manager deploys agent services; user chooses placement policy and resource reservations.
5. Preflight shows host/Docker/storage/network compatibility and required corrections.
6. Review and create database cluster, routing and certificates.
7. Set backup policy and verify recovery.

Recovered managers first show live-state differences and require approval before changing managed resources. Users do not manually install agent services.

## Management flows

- Settings show scope, per-node overrides, compatibility, restart requirement and resulting plan.
- Migration shows measured estimate, source compatibility, data/account verification and explicit cutover; address/port preservation is checked.
- Updates show DB channel, supported path, health checks and approval for major changes.
- Backup browser shows temporary restore progress, expiry, resource use and selective restore preview.
- Database creation offers an accompanying generated user/password; later management supports imports/exports.
- phpMyAdmin defaults to authorized automatic login, with separate credentials as an alternative.
- Dashboard editor supports personal/shared dashboards and templates with permission-filtered data.
- Archived metric retrieval displays loading, missing ranges and cache behavior transparently.

Keep normal operations straightforward while exposing plans, audit and expert settings. Use text and icons alongside status colors; support keyboard navigation, focus management, contrast and reduced motion. Technical implementation details belong in diagnostics unless needed for a user decision.

## Transparency and guided building (proposal)

Proposed UX for X03 and the scope extension; nothing here is implemented. Galera flows stay primary and follow the same principles.

- **Plan preview ("show what will happen").** Every mutating action ends in a review step listing ordered steps, affected resources (create/change/remove), required approvals, expected interruption and rollback limits. The plan shown is the plan executed; after execution the operation view links each step to its audit record.
- **Export as code.** Desired state of a service, platform or recipe can be exported (for example YAML/JSON specification, generated Swarm stack or Kubernetes manifests, Dockerfile/build recipe, Traefik dynamic configuration). Secrets are exported as references only. Import of exported code is a later option, not promised.
- **Expert view.** A read-only tab per resource shows generated artefacts, observed state, drift and the equivalent CLI/API calls. It explains; it does not replace guided forms with raw editing.
- **Web server composer wizard.** Steps: base (Nginx version from a curated list, architecture) → PHP (version, extensions as searchable checklist with compatibility hints, `php.ini` values with defaults and help) → configuration (template-based Nginx config with a validated editor, includes and site files) → build target (managed or external registry) → review (recipe, generated Dockerfile, expected image size, scan policy) → build → deploy/update. Invalid combinations are blocked with an explanation instead of failing during the build.
- **Traefik route editor (sketch).** List of routers with host/path rules, entry points, TLS/certificate source and attached middlewares; a service picker limited to managed services; a diff of the generated dynamic configuration before applying; conflicts (overlapping rules, missing certificates) shown inline.
- **Platform and update wizards.** Cluster provisioning shows host prerequisites, the exact commands per host and a teardown path. Docker engine updates show the compatibility matrix, the host order, the drain/quorum checks per host and who performs restarts, according to the decision still open in [ENTSCHEIDUNGEN.md](ENTSCHEIDUNGEN.md#open-decisions-from-the-scope-extension).

## R1 web UI (implemented in `web/`)

Verified implementation, not the full roadmap. Routes:

- Shell (`src/routes/(app)`): fixed desktop sidebar, mobile drawer (bits-ui dialog with focus trap, Escape closes), top bar with language switch and sign-out, skip link, focus moves to the page heading after client-side navigation, `prefers-reduced-motion` disables animation and transitions.
- Pages: Overview (process health/version, swarm/cluster counts by state), Swarms, Clusters, Cluster detail, Operations (timeline, newest first), Events, Settings (language, light/dark/system, accent color, add passkey). Hosts, Databases, Backups, Dashboards, Notifications and the manager settings card are built as described under "Pages added in the second UI batch" below.
- Auth (`src/routes/(auth)`): `/setup` (setup token, account, passkey registration, recovery codes shown once with copy/download and confirmation), `/login` (discoverable passkey), `/recover` (recovery code), `/recover/link` (recovery URL token), `/recover/passkey` (re-enrolment in the restricted recovery session).

Rules applied:

- States always render as icon plus text. Unrecognized or missing values render as "Unknown", never as healthy. Missing `observedAt` renders as "freshness unknown".
- Errors are shown by coarse class only (401, 403 with `origin_rejected` or `passkey_enrollment_required`, 404, 409, 429 with Retry-After, 503, network, invalid response); server text is never displayed. A 401 from any request returns the browser to sign-in.
- Responses are checked by hand-written runtime guards (`src/lib/api.ts`); responses that break the contract are rejected instead of being partly shown.
- POSTs are plain same-origin `fetch` calls; the browser adds Origin and Sec-Fetch-Site, which the manager checks. No custom CSRF token exists.
- WebAuthn options and credentials go through `src/lib/webauthn.ts`, which uses `PublicKeyCredential.parseCreationOptionsFromJSON`, `parseRequestOptionsFromJSON` and `toJSON` when the browser has them, and otherwise falls back to an equivalent conversion that is covered by tests.
- Language, theme and accent are stored per browser (`localStorage`). The manager has no per-user preference store yet, so U08 "per user" is only partly met.
- Development mock mode (`npm run dev:mock`, `VITE_MOCK=1`) exists only behind `import.meta.env.DEV`. It shows a "MOCK DATA" banner, and production builds contain neither its fixtures nor the banner.

Proposed, not yet backed by the CLI: recovery links have the form `/recover/link#account=<id>&token=<token>`. The fragment never reaches the server, is removed from the address bar after it is read, and the single-use token is redeemed only after an explicit click.

Contract gaps (shown as "Unknown (not reported)" until `api/openapi.yaml` carries them): cluster endpoint, node roles, last verified restore and automation policy. Cluster nodes have no `role` field yet.

### Pages added in the second UI batch

Verified: `npm run check`, `npm test` and `npm run build` pass; rendering was not checked in a browser (Playwright was not available). Only `/swarms`, `/clusters`, `/clusters/{id}`, `/operations`, `/events`, `/version` and the auth routes exist in `api/openapi.yaml`. Everything else below is an **assumed contract**, implemented against these names. A 404 or 503 on such an endpoint renders as "API not available" (`ApiGate`); no placeholder data is shown.

- Hosts: `GET /api/v1/hosts` returns `{items: Host[]}`. Host uses the JSON names of `manager.HostRecord` (`swarm`, `nodeId`, `hostname`, `state`, `availability`, `enrollment` = discovered|enrolled|revoked, `spare`, `nanoCpus`, `memoryBytes`, `observedAt`) plus proposed `agentVersion` and `lastSeen`. Absent values render as unknown.
- Cluster creation wizard (Clusters, "New cluster"): `POST /api/v1/clusters` with the body of `manager.CreateClusterRequest`: `swarm`, `name`, `size` (3|5), `image`, `network`, `rootPasswordSecret` (a Swarm secret name, never the password), optional `gcacheSize`, `resources: {limits, reservations}` and `placement: {allowlist?, excluded?, preferred?, reserve}` with amounts in `galera.ResourceAmount` names (`milli_cpu`, `memory_bytes`), optional `autoReplace`. The response is assumed to be `{id}` of the started operation. Placement precedence shown to the user follows `galera.PlacementPolicy`: allowlist, exclusion, preference, automatic scoring; allowlisted and excluded together is a manager-side conflict. The preflight page is a client-side preview from `GET /hosts` (enrolled, ready, active, not spare, known capacity covering demand plus reserve); it cannot see load from other clusters and says so. Error codes recognised on create: `no_capacity`, `policy_conflict`, `settings_incomplete`, `cluster_exists`, `not_leader` (error codes outside the `knownCodes` allowlist in `api.ts` are dropped).
- Cluster detail: optional proposed fields `nodes[].role`, `nodes[].observedAt`, `endpoints[]` (`{name?, host, port}`), `lastBackupAt`, `lastVerifiedRestoreAt`, `automationPolicy`. Replace node: `POST /api/v1/clusters/{id}/nodes/{slot}/replace`, success `{id}`; the error code `requires_guided_recovery` (mapped from `galera.ErrRequiresGuidedRecovery`, assumed HTTP 409) is shown as explanatory text, not as a failure. Full-outage diagnosis (read-only): `GET /api/v1/clusters/{id}/outage-diagnosis` returning `galera.Diagnosis` (`candidate`, `positions`, `reasons`, `risks`, `requires_approval`). Reasons and risk details are manager-generated diagnostic text and are displayed as given; nothing in this view changes cluster state.
- Backups: clusters come from `GET /clusters`; `GET /api/v1/backups` returns `{items: [{cluster, policy?, lastBackupAt?, lastVerifiedRestoreAt?}]}`. The restore wizard is a skeleton (select, inspection via `POST /api/v1/restores/inspect` `{cluster}` which is read-only, approval). The approval button is permanently disabled because no approval endpoint is assumed.
- Dashboards: `GET/POST /api/v1/dashboards`, `PUT/DELETE /api/v1/dashboards/{id}`; a dashboard is `{id, name, shared, owner?, updatedAt?, panels[{id, kind: line|stat, title, query, unit?}]}`; `shared=false` is personal. Panels call `POST /api/v1/metrics/query` with `{query, from, to, step}` (RFC 3339 times, step in seconds) and expect `{series: [{labels?, points: [[unixSeconds, value|null]]}]}`. `null` values and holes larger than 1.5 steps are drawn as hatched gap bands and never interpolated (`src/lib/chart.ts`, `LineChart.svelte`, hand-written SVG, colors from theme tokens, `role="img"` with a textual summary). A stat tile shows a value only if its newest real point is at most three steps old; otherwise it is unknown.
- Notifications: `GET /api/v1/notifications` (`{items: [{id, time, severity, message, cluster?, read, acknowledged}]}`), `POST /api/v1/notifications/{id}/read` and `/ack`, `GET/PUT /api/v1/settings/notifications` with `{webhook: {enabled, url?, secretSet}, email: {enabled, recipients[]}}`. The webhook secret is write-only: sent in the PUT only when typed, never returned or shown.
- Databases lists the clusters' engines and endpoints (reported or "not reported"); logical databases and users are not in any API yet. Manager settings shows `GET /version`; editing is not available.
