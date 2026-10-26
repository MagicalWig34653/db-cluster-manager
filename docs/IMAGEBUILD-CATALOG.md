# Web server image catalog provenance

Status: 2026-10-25, offline code review. `internal/imagebuild/catalog.go` is
the authoritative selectable catalog. `Catalog.Evidence` reports
`unverified` for every selectable entry. No upstream documentation snapshot,
Alpine package index, Docker image manifest or real build is available in
this worktree. A name that looks plausible is not `verified-by-doc` evidence.
The browser's catalog is a separate copy and needs a drift check before
production use.

| Kind | Selectable entries | Provenance |
| --- | --- | --- |
| nginx base tags | `1.26-alpine`, `1.27-alpine` (digest required) | unverified for each tag |
| PHP FPM prefixes | `php82`, `php83`, `php84` (`-fpm`) | unverified for each package |
| PHP extensions on 8.2 | bcmath, curl, exif, gd, intl, mbstring, mysqli, opcache, pdo_mysql, pdo_pgsql, pgsql, redis, sodium, xml, zip | unverified for each `php82-` package |
| PHP extensions on 8.3 | bcmath, curl, exif, gd, gmp, intl, mbstring, mysqli, opcache, pdo_mysql, pdo_pgsql, pgsql, redis, sodium, xml, zip | unverified for each `php83-` package |
| PHP extensions on 8.4 | bcmath, curl, exif, gd, gmp, intl, mbstring, mysqli, opcache, pdo_mysql, pdo_pgsql, pgsql, sodium, xml, zip | unverified for each `php84-` package |
| Extra Alpine packages | ca-certificates, curl, ghostscript, imagemagick, msmtp, tzdata | unverified for each package |

The `gmp` exclusion on PHP 8.2 and `redis` exclusion on PHP 8.4 are also
unverified assumptions. To change an entry to `verified-by-doc`, record an
upstream nginx image tag or Alpine package-index page for the exact Alpine
release and architecture, the consulted date and the package/version shown.
PHP's upstream extension documentation alone does not establish that an
Alpine binary package exists. Check the PHP FPM executable and configuration
paths against the selected image, then perform an isolated build before
claiming the composer works. This review made no network or infrastructure
contact and does not close that gate.
