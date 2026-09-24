# Copyright 2026 The Aetherize Authors.
# SPDX-License-Identifier: Apache-2.0

CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
# Matches the controller-gen.kubebuilder.io/version stamped in the CRDs.
CONTROLLER_GEN_VERSION ?= v0.21.0
PROJECT_DIR := $(shell pwd)

.PHONY: all
all: generate manifests vet build build-plugin build-installer

$(CONTROLLER_GEN):
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy methods for API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./bridge/api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD manifests under config/crd/bases (and the chart's copy)
	$(CONTROLLER_GEN) crd paths=./bridge/api/... output:crd:dir=config/crd/bases
	cp config/crd/bases/harbor.aetherize.io_harboraccesses.yaml $(PROJECT_DIR)/charts/harbor-bridge/crds/

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

.PHONY: build-plugin
build-plugin: ## Build the kubelet credential-provider plugin into bin/harbor-bridge-plugin
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/harbor-bridge-plugin ./plugin

.PHONY: build-installer
build-installer: ## Build the node installer into bin/harbor-bridge-installer
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/harbor-bridge-installer ./installer

.PHONY: build-all
build-all: ## Compile-check every package
	go build ./...

.PHONY: test
test: ## Run unit tests (envtest tests skip cleanly when KUBEBUILDER_ASSETS is unset)
	go test ./...

ENVTEST_K8S_VERSION ?= 1.34.x
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

# Optional pin for the Harbor Helm chart version the e2e harness installs.
# Empty → the harness default (test/e2e/modules/harbor/main.tf). Set e.g.
# `make e2e HARBOR_CHART_VERSION=1.13.5` to run the chain against an older
# Harbor; this is what the harbor-compat CI matrix drives (ADR-0020). The
# value is a *chart* version, not a Harbor app version (chart 1.N → Harbor 2.(N-4)).
HARBOR_CHART_VERSION ?=
e2e_harbor_var := $(if $(HARBOR_CHART_VERSION),TF_VAR_version_harbor=$(HARBOR_CHART_VERSION),)

.PHONY: e2e
e2e: ## Run the full e2e harness — fresh kind cluster, harbor, chart, pull/push assertions (~5 min). HARBOR_CHART_VERSION=<chart> pins Harbor.
	cd test/e2e && tofu init -upgrade -no-color && $(e2e_harbor_var) TF_VAR_pause_after_pull=false tofu test -verbose

.PHONY: e2e-pause
e2e-pause: ## Run e2e but pause AFTER the assertions — `rm test/e2e/.tofu-sleep-*` to continue
	cd test/e2e && tofu init -upgrade -no-color && $(e2e_harbor_var) TF_VAR_pause_after_pull=true tofu test -verbose

.PHONY: e2e-gke
e2e-gke: ## Run the GKE e2e harness (ADR-0022). CREATES BILLED GCP RESOURCES in $$GOOGLE_PROJECT (zonal spot GKE cluster, Artifact Registry repo, static IP); destroyed at the end of the run. Needs gcloud auth (application-default + docker) and docker. Never runs in CI.
	@test -n "$$GOOGLE_PROJECT" || (echo "set GOOGLE_PROJECT to the GCP project the harness may bill" && exit 1)
	cd test/e2e-gke && tofu init -upgrade -no-color && $(e2e_harbor_var) TF_VAR_gcp_project=$$GOOGLE_PROJECT TF_VAR_pause_after_pull=false tofu test -verbose

.PHONY: e2e-gke-pause
e2e-gke-pause: ## GKE e2e, but pause AFTER the assertions — `rm test/e2e-gke/.tofu-sleep-*` to continue
	@test -n "$$GOOGLE_PROJECT" || (echo "set GOOGLE_PROJECT to the GCP project the harness may bill" && exit 1)
	cd test/e2e-gke && tofu init -upgrade -no-color && $(e2e_harbor_var) TF_VAR_gcp_project=$$GOOGLE_PROJECT TF_VAR_pause_after_pull=true tofu test -verbose

.PHONY: proxy
proxy: ## Expose the cluster's apiserver at http://localhost:8001 so the bridge can fetch the JWKS off-cluster
	@echo "Run this in a separate terminal; leave it running while \`make run-local\` is active."
	@echo "Then set BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks when invoking run-local."
	kubectl proxy --port=8001

.PHONY: run-local
run-local: ## Run the bridge against $KUBECONFIG with a self-signed cert in /tmp/bridge-tls
	@test -n "$$BRIDGE_CLUSTER_NAME" || (echo "set BRIDGE_CLUSTER_NAME" && exit 1)
	@test -n "$$BRIDGE_NAMESPACE" || (echo "set BRIDGE_NAMESPACE" && exit 1)
	@test -n "$$BRIDGE_OIDC_ISSUER" || (echo "set BRIDGE_OIDC_ISSUER" && exit 1)
	@test -n "$$BRIDGE_AUDIENCE" || (echo "set BRIDGE_AUDIENCE (the audience your HarborAccess objects name)" && exit 1)
	@test -n "$$BRIDGE_HARBOR_URL" || (echo "set BRIDGE_HARBOR_URL" && exit 1)
	@test -n "$$BRIDGE_HARBOR_ADMIN_DIR" || (echo "set BRIDGE_HARBOR_ADMIN_DIR" && exit 1)
	@if echo "$$BRIDGE_OIDC_ISSUER" | grep -q cluster.local && [ -z "$$BRIDGE_OIDC_JWKS_URL" ]; then \
		echo ""; \
		echo "BRIDGE_OIDC_ISSUER looks like a cluster-internal URL but BRIDGE_OIDC_JWKS_URL is unset."; \
		echo "When running off-cluster the bridge cannot resolve cluster.local hostnames."; \
		echo "Start \`make proxy\` in another terminal and set:"; \
		echo "  export BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks"; \
		echo ""; \
		exit 1; \
	fi
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

# Every Go fuzz target, as <package>:<function>. `make fuzz` fails when a
# Fuzz function exists that this list does not name.
FUZZ_TARGETS ?= \
	./installer:FuzzMergeProvider \
	./installer:FuzzMergeExtraArgs \
	./installer:FuzzKubeletCmdline \
	./bridge/controlplane/harbor:FuzzRobotName_Injective \
	./bridge/internal/robotsecret:FuzzName_Injective \
	./bridge/dataplane:FuzzJSONAudience \
	./bridge/dataplane:FuzzCachedKeySet_VerifySignature
FUZZTIME ?= 10s

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each (default 10s); plain `go test` runs only their seed corpora
	@set -e; \
	listed=$$(for t in $(FUZZ_TARGETS); do echo "$${t##*:}"; done | sort); \
	found=$$(grep -rhoE '^func Fuzz[A-Za-z0-9_]+' --include='*_test.go' . | sed 's/^func //' | sort); \
	if [ "$$listed" != "$$found" ]; then \
		echo "FUZZ_TARGETS does not match the Fuzz functions in the tree."; \
		echo "listed:"; echo "$$listed"; echo "found:"; echo "$$found"; \
		exit 1; \
	fi; \
	for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; \
		echo "== $$fn ($$pkg, $(FUZZTIME))"; \
		go test -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) "$$pkg"; \
	done

.PHONY: verify-release-notes
verify-release-notes: ## Prove the semantic-release plugins pinned in release.yml render release notes (needs npm)
	./hack/check-release-notes.sh

.PHONY: verify-plugin-isolation
verify-plugin-isolation: ## Enforce ADR-0015: plugin must not pull k8s.io or sigs.k8s.io packages
	@count=$$(go list -deps ./plugin/... 2>/dev/null | grep -cE '^(k8s\.io|sigs\.k8s\.io)' || true); \
	if [ "$$count" != "0" ]; then \
		echo "ERROR: plugin imports k8s.io / sigs.k8s.io packages (violates ADR-0015):"; \
		go list -deps ./plugin/... | grep -E '^(k8s\.io|sigs\.k8s\.io)'; \
		exit 1; \
	fi

.PHONY: verify-generated
verify-generated: generate manifests ## Fail if the CRDs / deepcopy code drift from the Go API types
	@git diff --exit-code -- bridge/api config/crd charts/harbor-bridge/crds || \
		{ echo "generated files are stale — run 'make generate manifests' and commit the result"; exit 1; }

.PHONY: verify-installer-isolation
verify-installer-isolation: ## Enforce ADR-0021: installer may pull sigs.k8s.io/yaml (+ go.yaml.in) but nothing else from k8s.io / sigs.k8s.io
	@bad=$$(go list -deps ./installer/... 2>/dev/null | grep -E '^(k8s\.io|sigs\.k8s\.io)' | grep -v '^sigs\.k8s\.io/yaml$$' || true); \
	if [ -n "$$bad" ]; then \
		echo "ERROR: installer imports k8s.io / sigs.k8s.io packages beyond sigs.k8s.io/yaml (violates ADR-0021):"; \
		echo "$$bad"; \
		exit 1; \
	fi

# ====================================================================
# Helm chart targets (Phase 5)
# ====================================================================

CHART_DIR ?= charts/harbor-bridge
CHART_TESTS_DIR ?= $(CHART_DIR)/tests
GOLDEN_DIR ?= $(CHART_TESTS_DIR)/golden

# Each golden case is <values file suffix>:<golden file>. The single list
# drives lint, golden diff, and golden update so they cannot drift; the
# release config (.releaserc.json) commits every tests/golden/*.yaml.
CHART_CASES ?= complete:default mtls:mtls install-none:none plugin-disabled:plugin-disabled plugin-namespace:plugin-namespace
HELM_TEMPLATE = helm template harbor-bridge $(CHART_DIR) --kube-version 1.34.0 --namespace harbor-bridge-system

.PHONY: chart-lint
chart-lint: ## helm lint the chart against every test values file
	@set -e; for c in $(CHART_CASES); do \
		helm lint $(CHART_DIR) -f $(CHART_TESTS_DIR)/values-$${c%%:*}.yaml; \
	done

.PHONY: chart-golden
chart-golden: ## Diff current render against the checked-in golden files
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for c in $(CHART_CASES); do \
		v=$${c%%:*}; g=$${c##*:}; \
		$(HELM_TEMPLATE) -f $(CHART_TESTS_DIR)/values-$$v.yaml > "$$tmp/$$g.yaml"; \
		diff -u -B $(GOLDEN_DIR)/$$g.yaml "$$tmp/$$g.yaml" || { echo "golden mismatch for values-$$v.yaml — run 'make chart-golden-update' if intentional"; exit 1; }; \
	done; echo "golden render unchanged"

.PHONY: chart-golden-update
chart-golden-update: ## Re-capture golden files after intentional template changes
	@set -e; for c in $(CHART_CASES); do \
		$(HELM_TEMPLATE) -f $(CHART_TESTS_DIR)/values-$${c%%:*}.yaml > $(GOLDEN_DIR)/$${c##*:}.yaml; \
	done; echo "golden files refreshed; commit them after review"

.PHONY: chart-test-required
chart-test-required: ## Verify each required value gates install
	@$(CHART_TESTS_DIR)/test-required-values.sh

.PHONY: chart-test
chart-test: chart-lint chart-test-required chart-golden ## Full chart test suite (lint + required-value + golden)
	@echo "chart-test passed"
