# E17 part b: image composer catalog against real repositories and real builds (ADR 0026, X17)

Run: 2026-10-31 on lab VM `dbcm-lab-m1` (Docker 29.9.2, arm64), base images pulled by tag and pinned by digest: `debian:bookworm-slim`, `debian:trixie-slim`, `ubuntu:22.04`, `ubuntu:24.04`, `alpine:3.20/3.21/3.22`, `almalinux:9`, `rockylinux:9`. Harness: `cmd/composelab` (host side: prints every package spec the catalog can emit, or renders a recipe) and two shell loops in the VM. Recipes: `tests/p0/e17/*.json` (digests are the pulled ones of that day). Raw results: `e17/pkgaudit-before-*.tsv`, `e17/pkgaudit-after-*.tsv` (`OK` / `MISSING <reason>` per spec).

## 1. Package audit of the whole catalog

For each of the 9 base releases the harness enumerates every `(component, version, feature)` package spec for that release exactly as the generator renders it for the family (`name=1.22.*` for apt, `name~=1.26` for apk, plain name plus `dnf module enable` for dnf) and asks the package manager inside the real base image to **simulate the install** (`apt-get install -s`, `apk add -s`, `dnf install --assumeno` after enabling the module streams). Negative controls in the same loop (a bogus package, a non-existent version constraint, a non-existent module stream) were reported as missing on all three families, so a green line means the name and the version constraint exist.

| Release | Specs | Missing before | Missing after |
| --- | --- | --- | --- |
| Debian 12 / 13 | 53 / 58 | 0 | 0 |
| Ubuntu 22.04 / 24.04 | 54 / 57 | 1 (`libnginx-mod-http-brotli-filter`) | 0 |
| Alpine 3.20 / 3.21 / 3.22 | 119 / 162 / 162 | 0 | 0 |
| AlmaLinux 9 / Rocky 9 | 97 / 97 | 3 | 0 |

Everything else - nginx 1.18 to 1.28 on its distributions, PHP 8.1 to 8.4 and all 40 extensions on every offered release, Apache, Caddy, Node, Python, Java, SFTP, Composer, database clients, backup tools - resolved. The catalog is therefore accurate for package names and version prefixes; the three wrong entries were corrected:

1. `nginx` brotli module on **Ubuntu 22.04**: no such package. New catalog field `without` on a support entry (`"without": ["brotli"]`, validated against the component's features); recipe validation and the variant matrix refuse the feature there, Debian and Ubuntu 24.04 keep it. `TestBrotliIsRefusedOnUbuntu2204`.
2. **`ImageMagick` and `msmtp` on AlmaLinux/Rocky 9**: not in the base repositories (EPEL). The `rhel` mapping was removed, so the options are refused on dnf bases (`TestRhelBasesDoNotOfferEpelOnlyUtilities`).
3. **`curl` on AlmaLinux/Rocky 9**: the base image ships `curl-minimal`, `dnf install curl` fails with a package conflict. The `rhel` mapping is now empty (built into the base), like PHP's `curl` there (`TestCurlUtilityAddsNoPackageOnRhel`).

The catalog version is `2026.10.31-1`; the embedded catalog and the web mock catalog were updated together. `evidence` stays `unverified` on purpose: the validator accepts nothing else, and a package simulation is not a build of every combination.

## 2. Real builds (generated Dockerfiles, `docker build` in the VM)

| Image | Recipe | Result |
| --- | --- | --- |
| Debian 12 + nginx 1.22 + PHP 8.2 (10 extensions) | `debian12-nginx-php` | build and start work after two generator fixes (below); user `app` (10001), `GET /index.php` 200 with PHP 8.2.35 and the selected extensions, `GET /dbcm-ping` 200 |
| AlmaLinux 9 + nginx 1.26 (module stream) + PHP 8.3 | `alma9-nginx-php` | same: PHP 8.3.36, 200 and 200. A first recipe with the `redis` extension was refused by validation (not available on AlmaLinux), as the catalog says |
| Debian 12 + nginx + database clients (mariadb-client, postgresql-client, redis-cli, sqlite3) + system | `debian12-dbclients` | built; `mariadb`, `mysql`, `psql`, `redis-cli`, `sqlite3`, `nginx` present |
| AlmaLinux 9 + nginx + the same clients | `alma9-dbclients` | built; all six present |

Cache and layer sizes were not recorded. The images were run by `docker run` in the VM, not deployed as Swarm services and not built through the manager's build service (E17 part e: the module `image-build` needs `manager.imageBuild`, not configured).

### Generator bugs found by the real builds (fixed, golden files updated)

1. **Build check `php-fpm -t` failed with `[pool www] user has not been defined`** on both distributions: the build runs as root, the generated pool has no `user`. The check now runs `php-fpm -R -t` (allow root only for the check; the runtime stays user 10001). All FPM golden files changed.
2. **`nginx -t` left `/tmp/nginx.pid` and `/tmp/nginx-*` temp directories owned by root in the image layer**, so the container (user 10001) died at start with `open() "/tmp/nginx.pid" failed (13: Permission denied)` - every generated nginx image was unstartable. The check now removes them in the same layer. Both bugs passed 100% of the golden-file and fake-based tests.

## Not verified

Other recipe combinations beyond these four (Apache, Caddy, Node, Python, Java, Minecraft, SFTP and job-runner images were not built); Alpine, Ubuntu and Debian 13 builds (package simulation only); amd64 (arm64 only); the build through the manager's build service with approval digest, buildx multi-platform and SBOM; running the images as Swarm services; PHP behaviour under load; file locations in the Apache/Caddy generators.
