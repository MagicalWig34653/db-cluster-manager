---
title: Image composer
---

# Image composer [Fakes and mock server only; no image has been built from it]

The image composer ([ADR 0026](../../decisions/0026-image-composer.md), requirement X17) lets you assemble the contents of a service image by selecting components instead of writing a Dockerfile: base distribution, nginx with version and settings, PHP with version, extensions, `php.ini` and PHP-FPM pool settings, Composer and a few utilities. The manager validates the choice, generates a Dockerfile and configuration files, shows them for review and builds only after an administrator approves the exact plan, through the existing image build service (the **Image builds** page lists the resulting jobs).

**Status.** Fakes and the development mock server only. The component catalog is marked `unverified`: package names and versions were written from knowledge and not checked against live repositories, and no image has been built from a generated recipe. The real build is the final check and may reject an entry. The manager does not wire the composer endpoints yet (503 until it is constructed with a build service).

## Use the page

Open **Image composer** in the navigation.

1. **Base image.** Name the image, choose distribution and release (Alpine 3.20, Debian 12, Ubuntu 24.04 LTS, AlmaLinux 9) and enter the base image digest (`sha256:` plus 64 hex characters). A floating tag is never built. Resolve the digest yourself, for example `docker buildx imagetools inspect docker.io/library/alpine:3.20`.
2. **Components.** Tick the components to include. The version list only shows versions that exist on the chosen release. Extensions are per distribution family. A component that needs another (Composer needs PHP) cannot be selected without it. Exactly one component is the main process: nginx; PHP-FPM is a companion started next to nginx, or the single main process when nginx is not selected.
3. **Settings.** Every component shows its allow-listed settings with the default, the allowed range and, where it applies, a hint. Only settings that differ from the default are sent. Settings that cannot apply are hidden (for example `opcache.*` without the opcache extension, the FastCGI timeout without PHP).
4. **Review.** On the right the manager's answer follows every change (about 300 ms after you stop typing): findings with their path, the variants per distribution, the generated Dockerfile and every configuration file (choose the file in the list), the package list, limits to know and the hardening and reproducibility notes. The recipe digest identifies exactly this recipe.
5. **Variants.** Each release is listed as *compatible* or *differs*, with what differs and the newest version available there. *Use this base* switches the base and moves versions to what the release offers; extensions or components it does not have are dropped, which you see immediately.
6. **Plan and build.** Choose a registry, repository and tag and click *Plan build*. The plan shows the steps, warnings and the plan digest. Tick the approval and click *Build image*. Any change to the recipe, target or platforms makes the plan stale. Planning and building need the administrator role; operators can preview recipes.

## What the generator guarantees

- **Same recipe, same output.** The Dockerfile and context files are byte-identical for the same normalized recipe, catalog version and generator version (golden-file tests). The image itself is identical only while the base digest and the distribution repositories are unchanged: packages are constrained to the major.minor version but their patch versions float.
- **Hardening defaults.** Base pinned by digest, non-root user (uid 10001), unprivileged port 8080 (nginx) or 9000 (FPM only), no secrets or build arguments, no download scripts, no third-party repositories, `server_tokens` off, security headers on, `expose_php`, `display_errors` and `allow_url_fopen` off by default, `allow_url_include` off and `cgi.fix_pathinfo=0` always. `nginx -t` and `php-fpm -t` run during the build.
- **No arbitrary shell.** Settings are typed (bool, whole number with range, one of a list, or text with a mandatory pattern). Unknown keys, wrong types, out-of-range numbers and anything with shell metacharacters in a pattern-checked field are refused with the field path.
- **One main process.** There is no supervisor in the image. If PHP-FPM dies while nginx keeps running, requests get 502; use the service health check. A PHP-FPM-only image listens on 9000 without authentication: put it on an isolated network.

## API

| Route | Role | Purpose |
| --- | --- | --- |
| `GET /api/v1/image-composer/catalog` | viewer | Distributions, components, matrix, features, settings schema (EN/DE labels) |
| `POST /api/v1/image-composer/preview` | operator | Validate and generate; invalid recipes return `valid: false` and `issues` |
| `POST /api/v1/image-composer/builds` | administrator | Without `approvalDigest`: the build plan (200); with the plan's digest: starts the build (202). A changed recipe or target gives 409 `approval_mismatch` |

A recipe has `name`, `base {distro, release, digest}`, optional `platforms` and `components [{id, version, features, settings}]`. The full schema is in `api/openapi.yaml`.

## Limits

Fakes only; the catalog is unverified; no real build, SBOM or scan (the SBOM stays `not_generated`, the package list is its intended input). Four distributions, 16 PHP extensions, no PECL or source builds, no custom nginx snippets, one PHP-FPM pool, one site. The page is verified against the mock server only (screenshots must carry the mock marker).
