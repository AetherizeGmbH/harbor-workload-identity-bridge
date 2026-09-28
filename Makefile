# Copyright 2026 The Aetherize Authors.
# SPDX-License-Identifier: Apache-2.0

PROJECT_DIR := $(shell pwd)
LOCALBIN ?= $(PROJECT_DIR)/bin

# Tool versions. Renovate bumps each one through the `# renovate:` comment
# directly above it (Makefile custom manager in renovate.json). A tool's
# file name carries its version, so a bump installs the new version
# instead of reusing a stale binary.

# Matches the controller-gen.kubebuilder.io/version stamped in the CRDs.
# renovate: datasource=go depName=sigs.k8s.io/controller-tools
CONTROLLER_GEN_VERSION ?= v0.21.0
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_GEN_VERSION)

# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION ?= v1.8.0
GOVULNCHECK ?= $(LOCALBIN)/govulncheck-$(GOVULNCHECK_VERSION)

# renovate: datasource=go depName=sigs.k8s.io/controller-runtime/tools/setup-envtest
SETUP_ENVTEST_VERSION ?= v0.25.1
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest-$(SETUP_ENVTEST_VERSION)

# go-install-tool installs package $(2) at version $(3) as file $(1). `go
# install pkg@version` checks the module against the Go checksum database.
define go-install-tool
@set -e; mkdir -p $(LOCALBIN); tmp=$$(mktemp -d); \
	GOBIN=$$tmp go install $(2)@$(3); \
	mv "$$tmp/$$(basename $(2))" $(1); rm -rf "$$tmp"
endef

.PHONY: all
all: generate manifests vet build build-plugin build-installer

$(CONTROLLER_GEN):
	$(call go-install-tool,$@,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

$(GOVULNCHECK):
	$(call go-install-tool,$@,golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

$(SETUP_ENVTEST):
	$(call go-install-tool,$@,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(SETUP_ENVTEST_VERSION))

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy methods for API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./bridge/api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD manifests under config/crd/bases (and the chart's copy)
	$(CONTROLLER_GEN) crd paths=./bridge/api/... output:crd:dir=config/crd/bases
	cp config/crd/bases/harbor.aetherize.io_harboraccesses.yaml config/crd/bases/nexus.aetherize.io_nexusaccesses.yaml \
		$(PROJECT_DIR)/charts/harbor-bridge/crds/

.PHONY: tidy
tidy: ## Resolve module dependencies
	go mod tidy

.PHONY: fmt
fmt: ## Run gofmt
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Report known vulnerabilities reachable from our code (Go's official scanner)
	$(GOVULNCHECK) ./...

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

.PHONY: envtest-setup
envtest-setup: $(SETUP_ENVTEST) ## Fetch kube-apiserver + etcd binaries for envtest
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

.PHONY: envtest
envtest: $(SETUP_ENVTEST) manifests ## Run envtest-backed integration tests
	@KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./bridge/controlplane/... ./bridge/cmd/... -run TestEnvtest -count=1 -v -timeout 180s

# Optional pin for the Harbor Helm chart version the e2e harness installs.
# Empty → the harness default (test/e2e/modules/harbor/main.tf). Set e.g.
# `make e2e HARBOR_CHART_VERSION=1.13.5` to run the chain against an older
# Harbor; this is what the harbor-compat CI matrix drives (ADR-0020). The
# value is a *chart* version, not a Harbor app version (chart 1.N → Harbor 2.(N-4)).
HARBOR_CHART_VERSION ?=
e2e_harbor_var := $(if $(HARBOR_CHART_VERSION),TF_VAR_version_harbor=$(HARBOR_CHART_VERSION),)

.PHONY: e2e
e2e: ## Run the full e2e harness — fresh kind cluster, harbor, chart, pull/push assertions (~5 min). HARBOR_CHART_VERSION=<chart> pins Harbor.
	cd test/e2e && tofu init -no-color && $(e2e_harbor_var) TF_VAR_pause_after_pull=false tofu test -verbose

.PHONY: e2e-pause
e2e-pause: ## Run e2e but pause AFTER the assertions — `rm test/e2e/.tofu-sleep-*` to continue
	cd test/e2e && tofu init -no-color && $(e2e_harbor_var) TF_VAR_pause_after_pull=true tofu test -verbose

.PHONY: e2e-gke
e2e-gke: ## Run the GKE e2e harness (ADR-0022). CREATES BILLED GCP RESOURCES in $$GOOGLE_PROJECT (zonal spot GKE cluster, Artifact Registry repo, static IP); destroyed at the end of the run. Needs gcloud auth (application-default + docker) and docker. Never runs in CI.
	@test -n "$$GOOGLE_PROJECT" || (echo "set GOOGLE_PROJECT to the GCP project the harness may bill" && exit 1)
	cd test/e2e-gke && tofu init -no-color && $(e2e_harbor_var) TF_VAR_gcp_project=$$GOOGLE_PROJECT TF_VAR_pause_after_pull=false tofu test -verbose

.PHONY: e2e-gke-pause
e2e-gke-pause: ## GKE e2e, but pause AFTER the assertions — `rm test/e2e-gke/.tofu-sleep-*` to continue
	@test -n "$$GOOGLE_PROJECT" || (echo "set GOOGLE_PROJECT to the GCP project the harness may bill" && exit 1)
	cd test/e2e-gke && tofu init -no-color && $(e2e_harbor_var) TF_VAR_gcp_project=$$GOOGLE_PROJECT TF_VAR_pause_after_pull=true tofu test -verbose

.PHONY: proxy
proxy: ## Expose the cluster's apiserver at http://localhost:8001 so the bridge can fetch the JWKS off-cluster
	@echo "Run this in a separate terminal; leave it running while \`make run-local\` is active."
	@echo "Then set BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks when invoking run-local."
	kubectl proxy --port=8001

# Self-signed serving cert for run-local: per user, inside the checkout
# (gitignored), owner-only. A fixed path under shared /tmp let another local
# user plant the key pair, and a cert made once with -days 1 was reused
# after it expired. The path is stable because the plugin trusts the cert
# as its CA bundle (HOW-TO-TEST.md Phase 5).
RUN_LOCAL_TLS_DIR ?= $(PROJECT_DIR)/.gen/run-local-tls

.PHONY: run-local-tls
run-local-tls: ## Create or renew run-local's self-signed cert in .gen/run-local-tls (renewed when it expires within an hour)
	@set -e; dir="$(RUN_LOCAL_TLS_DIR)"; \
	if [ -L "$$dir" ] || { [ -e "$$dir" ] && [ ! -O "$$dir" ]; }; then \
		echo "$$dir is a symlink or not owned by you; refusing to use it"; exit 1; \
	fi; \
	umask 077; mkdir -p "$$dir"; chmod 700 "$$dir"; \
	if [ -f "$$dir/tls.key" ] && openssl x509 -checkend 3600 -noout -in "$$dir/tls.crt" >/dev/null 2>&1; then \
		exit 0; \
	fi; \
	rm -f "$$dir/tls.crt" "$$dir/tls.key" "$$dir/tls.crt.new" "$$dir/tls.key.new"; \
	openssl req -x509 -newkey rsa:2048 -nodes -days 7 \
		-keyout "$$dir/tls.key.new" \
		-out "$$dir/tls.crt.new" \
		-subj "/CN=localhost" \
		-addext "subjectAltName=DNS:localhost,IP:127.0.0.1"; \
	mv "$$dir/tls.key.new" "$$dir/tls.key"; \
	mv "$$dir/tls.crt.new" "$$dir/tls.crt"; \
	echo "run-local-tls: new self-signed cert $$dir/tls.crt (valid 7 days)"

.PHONY: run-local
run-local: run-local-tls ## Run the bridge against $KUBECONFIG with the self-signed cert from run-local-tls
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
	@echo "serving $(RUN_LOCAL_TLS_DIR)/tls.crt (the plugin's HARBOR_BRIDGE_CA_BUNDLE)"
	BRIDGE_TLS_CERT_FILE=$(RUN_LOCAL_TLS_DIR)/tls.crt \
	BRIDGE_TLS_KEY_FILE=$(RUN_LOCAL_TLS_DIR)/tls.key \
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
# Fuzz function exists that this list does not name, or when an entry's
# package does not contain its function (`go test -fuzz` in the wrong
# package fuzzes nothing and still passes).
FUZZ_TARGETS ?= \
	./installer:FuzzMergeProvider \
	./installer:FuzzMergeExtraArgs \
	./installer:FuzzKubeletCmdline \
	./installer:FuzzNodeFiles_Injective \
	./bridge/controlplane/harbor:FuzzRobotName_Injective \
	./bridge/controlplane/nexus:FuzzNexusName_Injective \
	./bridge/controlplane/nexus:FuzzRenderMessage_WithholdsSecrets \
	./bridge/internal/robotsecret:FuzzName_Injective \
	./bridge/internal/nexussecret:FuzzNexusSecretName_Injective \
	./bridge/dataplane:FuzzJSONAudience \
	./bridge/dataplane:FuzzCachedKeySet_VerifySignature \
	./bridge/dataplane:FuzzRoute
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
		go test -list="^$$fn$$" "$$pkg" | grep -qx "$$fn" || { \
			echo "FUZZ_TARGETS names $$t, but $$pkg has no $$fn."; \
			exit 1; \
		}; \
	done; \
	for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; \
		echo "== $$fn ($$pkg, $(FUZZTIME))"; \
		go test -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) "$$pkg"; \
	done

.PHONY: verify-release-notes
verify-release-notes: ## Prove the semantic-release plugins pinned in package-lock.json render release notes (needs npm)
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

# Each golden case is <values file suffix>:<golden file>[:<release>:<namespace>]
# (release harbor-bridge in namespace harbor-bridge-system unless given). The
# single list drives lint, golden diff, and golden update so they cannot drift;
# the release config (.releaserc.json) commits every tests/golden/*.yaml.
CHART_CASES ?= complete:default mtls:mtls install-none:none plugin-disabled:plugin-disabled plugin-namespace:plugin-namespace second-instance:second-instance:harbor-bridge-eu:harbor-bridge-eu nexus:nexus
# CHART_CASE splits the case in the loop variable c into v (values file
# suffix), g (golden file), rel (release name) and ns (namespace).
CHART_CASE = IFS=:; set -- $$c; unset IFS; v=$$1; g=$$2; rel=$${3:-harbor-bridge}; ns=$${4:-harbor-bridge-system}
HELM_TEMPLATE = helm template "$$rel" $(CHART_DIR) --kube-version 1.34.0 --namespace "$$ns"

.PHONY: chart-lint
chart-lint: ## helm lint the chart against every test values file
	@set -e; for c in $(CHART_CASES); do \
		$(CHART_CASE); \
		helm lint $(CHART_DIR) --namespace "$$ns" -f $(CHART_TESTS_DIR)/values-$$v.yaml; \
	done

.PHONY: chart-golden
chart-golden: ## Diff current render against the checked-in golden files
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for c in $(CHART_CASES); do \
		$(CHART_CASE); \
		$(HELM_TEMPLATE) -f $(CHART_TESTS_DIR)/values-$$v.yaml > "$$tmp/$$g.yaml"; \
		diff -u -B $(GOLDEN_DIR)/$$g.yaml "$$tmp/$$g.yaml" || { echo "golden mismatch for values-$$v.yaml — run 'make chart-golden-update' if intentional"; exit 1; }; \
	done; echo "golden render unchanged"

.PHONY: chart-golden-update
chart-golden-update: ## Re-capture golden files after intentional template changes
	@set -e; for c in $(CHART_CASES); do \
		$(CHART_CASE); \
		$(HELM_TEMPLATE) -f $(CHART_TESTS_DIR)/values-$$v.yaml > $(GOLDEN_DIR)/$$g.yaml; \
	done; echo "golden files refreshed; commit them after review"

.PHONY: chart-test-required
chart-test-required: ## Verify each required value gates install
	@$(CHART_TESTS_DIR)/test-required-values.sh

.PHONY: chart-test
chart-test: chart-lint chart-test-required chart-golden ## Full chart test suite (lint + required-value + golden)
	@echo "chart-test passed"
