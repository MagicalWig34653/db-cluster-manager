# E17 part e: module registry in a real manager, API and UI (ADR 0030)

Run: 2026-10-31 against the manager of part c (real replicated metastore, real session, real agents behind the modules). The UI part used headless Chromium (Playwright) with the session cookie of the API client; no passkey ceremony in the browser.

## API (real manager)

| Case | Result |
| --- | --- |
| `GET /modules` | 10 modules with `enabled`, `missingConfig`, `missingModules`, `dependents`. At the start `patch` was on (config default `patch.enabled: true`), all others off; `image-build` showed `missingConfig [manager.imageBuild]`, `vuln` `[manager.vuln]`, `bootstrap` `[manager.serverBootstrap.agentRelease]`, `image-composer` `missingModules [image-build]` |
| `PUT /modules/nope` | `404 not_found` |
| `PUT` enable `image-build`, `image-composer`, `vuln` with the prerequisite missing | `409 module_blocked` each (GET gives the reason) |
| Routes of a disabled module (`/host-sources`, `/image-composer/catalog`, `/vuln/findings`, `/patch/hosts`) | `404 module_disabled` before any handler |
| Disable `patch`, then enable again | routes answered `404 module_disabled` while off; **after enabling both patch hosts were listed again** (host records survive; their policies and history were not re-read) |
| Disable `host-backups` with an existing source | list and `POST .../backups` `404 module_disabled`; after enabling the source `e17-data` was listed again |
| Enable an enabled module | `200`, idempotent |
| Without a session | `GET /modules` and `PUT /modules/{id}` `401` |
| Dependents case (disabling a module another one needs) | **not run**: no module whose prerequisite is satisfiable could be enabled in this configuration (`image-build` needs `manager.imageBuild`) |

## Bug found and fixed

`PUT /modules/patch` with the body `{}` returned `200` **and switched the module off**: the decoder read the missing `enabled` as `false`. The OpenAPI document requires the field. The handler now decodes a pointer and answers `400 bad_request` when it is absent or `null`, changing nothing (`TestModulePutRequiresEnabled`). The UI always sends the field, so the browser was not affected.

## UI (headless Chromium)

`/settings/modules` loaded with the 10 modules, their titles, descriptions, "Switched by admin on ..." lines and the reasons ("Missing configuration: manager.imageBuild", "Needs these modules first: Image build"). The switches of the four blocked modules were disabled (the 409 cases are prevented in the UI). Clicking the OS patch management switch sent `PUT /api/v1/modules/patch` -> 200 and reloaded the list; a second click switched it back (200). The navigation has no entry for patch management or host backups (there is no page for them), so nothing disappeared from the menu. Screenshots were not kept.

## Not verified

Roles other than the administrator (a viewer's `403` is covered by the in-process test); a follower replica, leader change during a switch (the in-process HA test only); a module with a satisfied prerequisite chain (image build and composer need a build configuration, not part of this run); the German UI strings; switching with several browsers open.
