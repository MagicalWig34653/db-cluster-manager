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
    done

# ---- runtime: static binaries, CA certificates, non-root, no shell
FROM ${RUNTIME_IMAGE}
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="db-cluster-manager" \
      org.opencontainers.image.description="Galera Cluster Manager (manager, agent, cli)" \
      org.opencontainers.image.source="https://github.com/MagicalWig34653/db-cluster-manager" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/manager /out/agent /out/cli /usr/local/bin/
# Distroless "nonroot" runs as uid/gid 65532; the image works with a read-only
# root filesystem (the manager currently writes no files).
USER 65532:65532
EXPOSE 8080
# The distroless image has no shell or curl; the manager probes itself.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/manager", "-healthcheck", "-listen", "0.0.0.0:8080"]
# Listens on all interfaces inside the container; publish the port on loopback
# (see deploy/compose.yaml) unless a TLS-terminating proxy is in front.
ENTRYPOINT ["/usr/local/bin/manager"]
CMD ["-listen", "0.0.0.0:8080"]
