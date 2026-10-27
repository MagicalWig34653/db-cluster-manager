# syntax=docker/dockerfile:1
# SPDX-License-Identifier: AGPL-3.0-only
#
# Multi-stage image: web UI (Node) -> Go binaries -> minimal runtime.
# Build context is the repository root:
#   docker buildx build -f deploy/Dockerfile \
#     --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=0.1.0 --build-arg COMMIT=$(git rev-parse --short HEAD) .
#
# Base images are pinned by tag AND digest (multi-arch index digests, resolved
# 2026-10-20). Refresh deliberately; never use "latest".
ARG NODE_IMAGE=node:24.22.0-alpine@sha256:44fce696f38ba8c987c4db89954051000bbd05ffd8d8659ba15ed4a8602e937f
ARG GO_IMAGE=golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# ---- web UI (architecture independent output; build on the builder's platform)
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# adapter-static writes to ../internal/webui/assets
RUN mkdir -p /src/internal/webui/assets && npm run build

# ---- Go binaries (cross-compiled on the builder's platform, CGO disabled)
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=web /src/internal/webui/assets/ ./internal/webui/assets/
RUN set -eu; \
    PKG=github.com/MagicalWig34653/db-cluster-manager/internal/version; \
    for c in manager agent cli; do \
      GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT}" \
        -o /out/${c} ./cmd/${c}; \
    done; \
    mkdir -p /out/data

# ---- restic (pinned release, SHA-256 verified; used only by opt-in host backups)
# The checksums come from the release's SHA256SUMS file (fetched 2026-10-27)
# and are pinned here. tests/p0/e11-restic.sh reads these same ARG lines and
# verifies the very same archives in a lab VM. The binary is a static Go
# executable. It is NOT run during the build (the stage runs on the builder's
# platform); the lab run covers linux/arm64 only.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS restic
ARG TARGETARCH
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256_AMD64=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c
ARG RESTIC_SHA256_ARM64=a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) sum="${RESTIC_SHA256_AMD64}" ;; \
      arm64) sum="${RESTIC_SHA256_ARM64}" ;; \
      *) echo "restic: unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out; cd /tmp; \
    wget -q -O restic.bz2 "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2"; \
    echo "${sum}  restic.bz2" | sha256sum -c -; \
    bunzip2 -c restic.bz2 > /out/restic; \
    chmod 0755 /out/restic; test -s /out/restic

# ---- runtime: static binaries, CA certificates, non-root, no shell
FROM ${RUNTIME_IMAGE}
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="db-cluster-manager" \
      org.opencontainers.image.description="cluster-manager (manager, agent, cli)" \
      org.opencontainers.image.source="https://github.com/MagicalWig34653/db-cluster-manager" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/manager /out/agent /out/cli /usr/local/bin/
# The agent's ExecRunner finds restic on PATH. Nothing uses it unless an
# operator opts into restic host backups (see docs/guides/en/22-restic.md).
# There is no ssh client here: SFTP repositories need a different image.
COPY --from=restic /out/restic /usr/local/bin/restic
# Managed-mode data directory. A new named volume mounted here starts with
# this directory's owner, so the non-root manager can write to it.
COPY --from=build --chown=65532:65532 /out/data /var/lib/dbcm
# Distroless "nonroot" runs as uid/gid 65532; the image works with a read-only
# root filesystem (managed mode writes only below its data directory, a volume).
USER 65532:65532
EXPOSE 8080
# The distroless image has no shell or curl; the manager probes itself.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/manager", "-healthcheck", "-listen", "127.0.0.1:8080"]
# Safe prototype default. For a public bind configure manager.tls (ACME,
# static certificate, or explicit allowPlainHttp behind a TLS reverse proxy).
ENTRYPOINT ["/usr/local/bin/manager"]
CMD ["-listen", "127.0.0.1:8080"]
