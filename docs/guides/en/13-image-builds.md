---
title: Image builds and registries (stub)
---

# Image builds and registries (stub)

[Guide index](index.md) | Previous: [Configuration reference](12-configuration-reference.md)

**Status: [Fakes] only.** The backend exists (plan preview, approval digest, build job records, registry client) and was tested against fakes. It is not wired into the manager, has no UI, and has never built or pushed a real image. Do not rely on it.

## What exists

- **Composition:** nginx version, optional PHP-FPM version, PHP extensions and `php.ini` values from allow-lists, structured nginx options, a size-limited custom nginx snippet, extra packages, platforms. The base image must be pinned by digest.
- **Plan before build:** `POST /image-builds` without `approvalDigest` returns the generated Dockerfile, context files, steps, warnings and an explanation. A build starts only with the digest of that plan.
- **Registries:** `POST /registries` registers an external GHCR, Docker Hub or generic registry. The password is stored as a secret and never returned or logged. A manager-operated `registry:2` is only a deploy specification.
- **Records:** each job keeps spec hash, base image digests, image digest, an SBOM placeholder (not generated) and a redacted log.

## Limits

No real build, push, SBOM, vulnerability scan or signature. Alpine package versions are not pinned. Package names and version catalog are unverified. The snippet check is a deny-list, not a parser. See [Known limits](10-known-limits.md) and [STATUS.md](../../STATUS.md).
