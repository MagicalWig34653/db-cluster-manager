---
title: Image composer
---

# Image composer [Fakes and mock server only; no image has been built from it]

The image composer ([ADR 0026](../../decisions/0026-image-composer.md), requirement X17) lets you assemble the contents of a service image by selecting components instead of writing a Dockerfile: base distribution, nginx with version and settings, PHP with version, extensions, `php.ini` and PHP-FPM pool settings, Composer and a few utilities. The manager validates the choice, generates a Dockerfile and configuration files, shows them for review and builds only after an administrator approves the exact plan, through the existing image build service (the **Image builds** page lists the resulting jobs).

**Status.** Fakes and the development mock server only. The component catalog (version 2026.10.29-2) is marked `unverified`: package names and versions were written from knowledge and not checked against live repositories, and no image has been built from a generated recipe. The real build is the final check and may reject an entry. The manager does not wire the composer endpoints yet (503 until it is constructed with a build service).

## Use the page

Open **Image composer** in the navigation.

1. **Base image.** Name the image, choose distribution and release (Alpine 3.20 to 3.22, Debian 12 and 13, Ubuntu 22.04 and 24.04 LTS, AlmaLinux 9, Rocky Linux 9) and enter the base image digest (`sha256:` plus 64 hex characters). A floating tag is never built. Resolve the digest yourself, for example `docker buildx imagetools inspect docker.io/library/alpine:3.20`.
2. **Components.** Tick the components to include. The version list only shows versions that exist on the chosen release. Extensions are per distribution family. A component that needs another (Composer needs PHP) cannot be selected without it. Exactly one component is the main process (nginx, Apache, Caddy, Node.js, Python with gunicorn, Java, SFTP or the job runner); selecting a second one switches the first off. PHP-FPM is a companion started next to a web server, or the single main process (FPM or a CLI worker script) when no other main process is selected; PHP does not combine with Node.js, Python, Java, SFTP or the job runner.
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

Fakes only; the catalog is unverified; no real build, SBOM or scan (the SBOM stays `not_generated`, the package list is its intended input). Seven bases, 39 PHP extensions from distribution packages, no PECL or source builds, no custom web-server snippets, one PHP-FPM pool, one site, no application code in the image (see the section on the catalog extension for what is deliberately missing). The page is verified against the mock server only (screenshots must carry the mock marker).

## Catalog extension (version 2026.10.29-2)

Everything below is generated from distribution packages and typed settings; none of it has been built or run (see the status above).

**Bases.** Alpine 3.20, 3.21 and 3.22, Debian 12 and 13 (trixie), Ubuntu 22.04 and 24.04, AlmaLinux 9 and Rocky Linux 9 (the same dnf module streams as AlmaLinux). Not every component exists on every base: the version list shows only what the catalog expects there, and *Variants* shows the rest. Components whose distribution version is already end of life are left out (Node.js on Debian 12 and Ubuntu); PHP 8.1, Python 3.10 and nginx 1.18 are offered with a warning.

**Web servers (main process).** nginx, Apache httpd (event MPM, generated minimal configuration) and Caddy 2 (admin endpoint and automatic HTTPS off, plain HTTP behind your proxy). All three run PHP-FPM as companion. Shared settings: server name, document root, maximum body size, compression, security header preset (`basic`, `strict` with HSTS, Permissions-Policy and COOP, `api` with no-store and a closed CSP), real client address from `X-Forwarded-For` or `X-Real-IP` limited to the trusted proxy ranges you list, access log on or off. nginx adds optional brotli (distribution module), a rate limit per client address with burst (answers 429) and `trustForwardedProto` (sets the PHP `HTTPS` flag when TLS ends at your proxy; enable it only when every request passes the proxy). TLS is never terminated in the image.

**PHP.** PHP 8.1 to 8.4 depending on the base; 39 extensions including `apcu`, `imagick`, `redis` and `memcached` where the distribution packages them (not on AlmaLinux/Rocky for `imagick`, `redis`, `memcached`); opcache settings including `validate_timestamps`, `revalidate_freq`, `max_accelerated_files`, `interned_strings_buffer` and the JIT (`opcache.jit`, buffer size), `apc.shm_size`, `max_file_uploads`, `realpath_cache_size`, `pm.process_idle_timeout`. Run mode **worker** makes a PHP CLI script (for example a queue consumer) the single main process; it cannot be combined with a web server. Several PHP-FPM pools are not offered: the image has one site (see the ADR addendum).

**Application runtimes (main process).** Node.js 20 or 22 (entry script, port, `NODE_ENV`, heap limit, optional npm), Python 3 with gunicorn (WSGI `module:callable`, workers, threads, timeout, trusted proxies, optional pip; not on AlmaLinux/Rocky) and Java 17 or 21 JRE (jar path, heap percentage, garbage collector, exit on out-of-memory). The application is **not** in the image: mount it at `/app` or build a derived image from this one. These runtimes cannot be combined with PHP.

**Storage and utility images.** *SFTP* (OpenSSH, internal SFTP subsystem only, port 2222, one account, no chroot): host key and authorized keys come as mounted secrets (`/run/secrets/...`), not from the image. *Scheduled job runner*: a generated shell loop that runs the preset `restic-backup` or `rclone-sync` at a fixed interval; repository, password file and rclone configuration come from the service environment or secrets. *Backup and sync tools* (restic, rclone, rsync, OpenSSH client, MinIO client on Alpine only) and *Database client tools* (MariaDB, PostgreSQL, redis-cli, SQLite) are package selections without a process.

**System settings.** Time zone (`TZ`), locale (`C.UTF-8`, `en_US.UTF-8`, `de_DE.UTF-8`; generated on Debian, Ubuntu and AlmaLinux/Rocky, only `LANG` on Alpine), additional CA certificates as PEM (each must be a CA certificate; private keys and leaf certificates are refused; they are installed into the system trust store) and a container health check: `http` (curl against the main web port or the runtime port) or `fpm-ping` (nginx with PHP-FPM asks PHP-FPM itself through a local-only ping endpoint, so a dead PHP-FPM is noticed).

**Deliberately not offered.** Go static binaries on `scratch` or distroless: those bases have no package manager and the composer has no way yet to bring a binary into the build context safely (a digest-pinned payload image would be the next step, a product decision). vsftpd and chrooted multi-user SFTP: both need root or PAM to authenticate and switch users, which the non-root rule forbids; plain FTP is also unencrypted and awkward behind Swarm routing. Several PHP-FPM pools: one site per image.

**Unverified facts.** Besides package names, the catalog assumes distribution-specific file locations (Apache module directories, nginx brotli module path) and package contents (for example that sshd runs as a non-root user and that the Caddy package supports the generated directives; Debian 12 and Ubuntu 24.04 probably ship an older Caddy that does not know `client_ip_headers`). Each generated image runs a configuration test where the tool offers one (`nginx -t`, `httpd -t`, `caddy validate`, `php-fpm -t`, `sshd -t`), so a wrong assumption fails the build, not the start. Release existence of Debian 13, Rocky/AlmaLinux 9 and PHP 8.4 was checked against endoflife.date on 2026-10-29; Alpine data could not be fetched. No package name was checked.
