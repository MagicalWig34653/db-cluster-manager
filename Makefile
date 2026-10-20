# SPDX-License-Identifier: AGPL-3.0-only
VERSION ?= 0.0.0-dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/MagicalWig34653/db-cluster-manager/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)

.PHONY: web build test check clean

web: ## Build the frontend into internal/webui/assets (embedded by the manager)
	cd web && npm ci && npm run build
	touch internal/webui/assets/.gitkeep # the build empties the directory; keep the tracked placeholder

build: web ## Build manager, agent and CLI into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test: ## Run Go and frontend tests
	go test ./...
	cd web && npm test

check: ## Static checks and documentation check
	gofmt -l . | (! grep .)
	go vet ./...
	cd web && npm run check
	python3 scripts/check_docs.py

clean:
	rm -rf bin web/.svelte-kit web/node_modules
	find internal/webui/assets -mindepth 1 ! -name .gitkeep -delete
