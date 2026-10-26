# cluster-manager {tag}

## Changes

{changes}
## Artifacts

Linux amd64 and arm64 archives contain manager, agent, CLI, build information, and CycloneDX SBOMs. SHA256SUMS covers all downloadable archives and standalone SBOMs. Multi-architecture manager and jobs images are published to GHCR with BuildKit SBOM and provenance attestations and keyless image signatures.

## Verification and limits

Check SHA256SUMS and the image signature before use. This release pipeline does not prove production readiness; review [installation and known limits](https://github.com/MagicalWig34653/db-cluster-manager/blob/{tag}/docs/INSTALL.md) before a lab trial.

{range}
