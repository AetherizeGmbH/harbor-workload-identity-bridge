# Copyright 2026 The Aetherize Authors.
# SPDX-License-Identifier: Apache-2.0

CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
PROJECT_DIR := $(shell pwd)

.PHONY: all
all: generate manifests vet build-all

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
build: ## Build the bridge binary into bin/bridge
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/bridge ./bridge/cmd

.PHONY: build-all
build-all: ## Compile-check every package
	go build ./...

.PHONY: test
test: ## Run unit tests (envtest tests skip cleanly when KUBEBUILDER_ASSETS is unset)
	go test ./...

ENVTEST_K8S_VERSION ?= 1.30.x
SETUP_ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest

$(SETUP_ENVTEST):
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.21

.PHONY: envtest-setup
envtest-setup: $(SETUP_ENVTEST) ## Fetch kube-apiserver + etcd binaries for envtest
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

.PHONY: envtest
envtest: $(SETUP_ENVTEST) manifests ## Run envtest-backed integration tests
	@KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./bridge/controlplane/... -run TestEnvtest -count=1 -v -timeout 120s

.PHONY: run-local
run-local: ## Run the bridge against $KUBECONFIG with a self-signed cert in /tmp/bridge-tls
	@test -n "$$BRIDGE_CLUSTER_NAME" || (echo "set BRIDGE_CLUSTER_NAME" && exit 1)
	@test -n "$$BRIDGE_NAMESPACE" || (echo "set BRIDGE_NAMESPACE" && exit 1)
	@test -n "$$BRIDGE_OIDC_ISSUER" || (echo "set BRIDGE_OIDC_ISSUER" && exit 1)
	@test -n "$$BRIDGE_HARBOR_URL" || (echo "set BRIDGE_HARBOR_URL" && exit 1)
	@test -n "$$BRIDGE_HARBOR_ADMIN_DIR" || (echo "set BRIDGE_HARBOR_ADMIN_DIR" && exit 1)
	@mkdir -p /tmp/bridge-tls
	@test -f /tmp/bridge-tls/tls.crt || \
		openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
			-keyout /tmp/bridge-tls/tls.key \
			-out /tmp/bridge-tls/tls.crt \
			-subj "/CN=localhost" \
			-addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
	BRIDGE_TLS_CERT_FILE=/tmp/bridge-tls/tls.crt \
	BRIDGE_TLS_KEY_FILE=/tmp/bridge-tls/tls.key \
	BRIDGE_LISTEN_ADDR=:8443 \
	BRIDGE_HEALTH_ADDR=:8081 \
	go run ./bridge/cmd

.PHONY: verify-package-isolation
verify-package-isolation: ## Enforce ADR-0002: controlplane must not import dataplane
	@if go list -deps ./bridge/controlplane/... 2>/dev/null | grep -q github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane; then \
		echo "ERROR: bridge/controlplane imports bridge/dataplane (violates ADR-0002)"; \
		exit 1; \
	fi
