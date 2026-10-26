---
title: Image builds and registries (stub)
---

# Image builds and registries (stub)

[Guide index](index.md) | Previous: [Configuration reference](12-configuration-reference.md)

**Status: [Fakes] only.** The backend exists (plan preview, approval digest, build job records, registry client) and is now wired into the manager with replicated storage (see below), but only tested against fakes: a fake `docker` command and `httptest` registries. The web UI has only run against the mock server, and no real image was ever built or pushed. Do not rely on it.

## What exists

- **Composition:** nginx version, optional PHP-FPM version, PHP extensions and `php.ini` values from allow-lists, structured nginx options, a size-limited custom nginx snippet, extra packages, platforms. The base image must be pinned by digest.
- **Plan before build:** `POST /image-builds` without `approvalDigest` returns the generated Dockerfile, context files, steps, warnings and an explanation. A build starts only with the digest of that plan.
- **Registries:** `POST /registries` registers an external GHCR, Docker Hub or generic registry. The password is stored as a secret and never returned or logged. A manager-operated `registry:2` is only a deploy specification.
- **Records:** each job keeps spec hash, base image digests, image digest, an SBOM placeholder (not generated) and a redacted log.

## In the manager (wired, fakes only)

- **Storage:** registries, build jobs and previewed plans live in the replicated metastore (collections `registries`, `image-jobs`, `image-plans`) and survive restarts and leader changes. A registry password is sealed with the data key (`keyFile` or `keyring`) in its own collection; registry records hold only a reference, and neither the API nor jobs, plans or logs contain it. At most 200 plans are kept (oldest removed).
- **Builder:** with a `manager.imageBuild` section ([configuration reference](12-configuration-reference.md#managerimagebuild), `manager init -image-build`) the manager runs `docker buildx build` as its own process, in the background after the approval. Without the section `POST /image-builds` answers 503; registries still work.
- **Restart:** a build interrupted by a restart or leadership change cannot continue. When a leader starts, jobs still `running` are marked `failed` ("interrupted").
- **Redaction:** the job log and error are redacted before they are stored (known secret values, basic-auth form, credential patterns; best effort).
- **Authorization:** `image_build` and `registry` are separate resources; viewers read, operators also plan and start builds, only administrators register registries. Every decision, also denials, is audited.

## Limits

The manager image has no docker CLI and the stack no BuildKit, so builds cannot work in the shipped deployment without extra work. The build runs on the leader's host as the manager process (access to the Docker socket). Registering a registry contacts it from the manager (credential check). Deleting or changing a registry and deleting jobs are not implemented. Plans are stored for audit but there is no API to read them yet.


No real build, push, SBOM, vulnerability scan or signature. Alpine package versions are not pinned. Every catalog entry is marked unverified after the offline review; see the [catalog provenance table](../../IMAGEBUILD-CATALOG.md). The snippet check is a deny-list, not a parser. See [Known limits](10-known-limits.md) and [STATUS.md](../../STATUS.md).

## In the web UI (mock only)

The **Image builds** page lists recorded builds (status, target image, image digest, SBOM placeholder) and has the web server composer: nginx and PHP version, PHP extensions (only those the catalog offers for the chosen PHP version), `php.ini` values, nginx options, an optional custom snippet, extra packages, platforms and the push target. **Show Dockerfile and plan** displays the generated Dockerfile, the other context files, the steps and warnings, and the plan digest. The build button stays disabled until you tick that you reviewed exactly this plan, and any later edit of the form invalidates the plan. The base image must be given by digest.

![Image composer with the rendered Dockerfile and plan](../../assets/screenshots/en-light-images.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

The **Registries** page lists registries (never a credential, only whether one is stored) and adds an external GHCR, Docker Hub or generic OCI registry. The password field is write-only: it is sent once and cleared from the page immediately.

![Registries page with the add form](../../assets/screenshots/en-light-registries.png)

*Screenshot from the development mock server (`npm run dev:mock`, invented sample data), not from a real manager.*

Both pages ran only against the mock server [Mock only].
