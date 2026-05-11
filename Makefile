# Copyright 2026 The Aetherize Authors.
# SPDX-License-Identifier: Apache-2.0

CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
PROJECT_DIR := $(shell pwd)

.PHONY: all
all: generate manifests vet build

.PHONY: generate
generate: ## Generate deepcopy methods for API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./bridge/api/...

.PHONY: manifests
manifests: ## Generate CRD manifests under config/crd/bases
	$(CONTROLLER_GEN) crd paths=./bridge/api/... output:crd:dir=config/crd/bases

.PHONY: tidy
tidy: ## Resolve module dependencies
	go mod tidy

.PHONY: fmt
fmt: ## Run gofmt
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: build
build: ## Build all packages
	go build ./...

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: verify-package-isolation
verify-package-isolation: ## Enforce ADR-0002: controlplane must not import dataplane
	@if go list -deps ./bridge/controlplane/... 2>/dev/null | grep -q github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane; then \
		echo "ERROR: bridge/controlplane imports bridge/dataplane (violates ADR-0002)"; \
		exit 1; \
	fi
