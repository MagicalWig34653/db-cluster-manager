# UI and user experience

## Reference and language

UI technology: Svelte and shadcn-svelte. The local `timetrack-pwa` reference was inspected through `package.json`, `components.json`, `src/routes/layout.css` and the application layout. No browser visual inspection was performed.

Observed visual direction: neutral Zinc surfaces, Inter typography, fine borders, rounded panels, light/dark modes, configurable accent colors, fixed desktop sidebar, mobile drawer, top bar and restrained translucent framing with accessible fallbacks. Treat these as visual inspiration, not an automatic source-code or license transfer.

UI supports German/English per user. Code and comments are English. User guides are bilingual. This project must remain understandable without access to the local reference repository.

## Navigation proposal

Overview · Swarms · Clusters · Hosts · Databases · Backups · Dashboards · Operations · Events · Settings.

Show cluster health, data freshness, endpoint, node roles, last verified restore and automation policy directly. Unknown observations must not look healthy. Explain blocking conditions and offer relevant next actions.

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

## R1 web UI (implemented in `web/`)

Verified implementation, not the full roadmap. Routes:

- Shell (`src/routes/(app)`): fixed desktop sidebar, mobile drawer (bits-ui dialog with focus trap, Escape closes), top bar with language switch and sign-out, skip link, focus moves to the page heading after client-side navigation, `prefers-reduced-motion` disables animation and transitions.
- Pages: Overview (process health/version, swarm/cluster counts by state), Swarms, Clusters, Cluster detail, Operations (timeline, newest first), Events, Settings (language, light/dark/system, accent color, add passkey). Hosts, Databases, Backups and Dashboards, plus manager settings, show an explicit "not available yet" state.
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
