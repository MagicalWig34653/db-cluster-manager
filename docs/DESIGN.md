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
