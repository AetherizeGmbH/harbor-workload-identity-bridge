# Project phases

Source of truth for what is done, what is next, and what is intentionally out of scope. Each phase ends with a working, tested deliverable. Cross-phase work is rejected — push to a later phase.

This document is written to survive context compaction. The detail sections below carry the load-bearing constraints, conventions, and constants — read them before resuming work in a phase.

## Current status (as of 2026-05-31)

- Phase 1: COMPLETE
- Phase 2 Slice A (CRD bump + control-plane foundations): COMPLETE
- Phase 2 Slice B (reconciler + janitor): COMPLETE
- Phase 2 follow-up (transient-retry + Secret-rebuild fixes): COMPLETE
- Phase 3 Slice A (OIDC validator): COMPLETE
- Phase 3 Slice B (Harbor token client + cache): **REMOVED in ADR-0013 pivot** — see "Architecture snapshot" below
- Phase 3 Slice C (handler): COMPLETE (rewritten by ADR-0013 pivot)
- Phase 3 Slice D (server + metrics + cmd/main.go): COMPLETE
- Phase 4 (plugin binary): COMPLETE
- Phase 5 (Helm chart): COMPLETE
- Phase 6 (kubelet-driven e2e + docs + v0.1.0): **IN PROGRESS — BLOCKED on kubelet silently aborting between credential-provider match and exec; see Phase 6 section.**

## Architecture snapshot (post-ADR-0013)

The bridge is one Go binary with two logically separated packages ([ADR-0002](adr/0002-bridge-control-plane-data-plane-split.md)):

**`bridge/controlplane`** — controller-runtime reconciler + periodic janitor:
- Reconciler manages a persistent Harbor robot per `HarborAccess` CR. Robot name is `bridge-<cluster>-<saNs>-<saName>`, deterministic, ownership-prefix scoped to the configured cluster.
- Robot password lives in a per-CR Kubernetes Secret `robot-<haNs>-<haName>` in the bridge namespace, type `Opaque`, data keys `username` + `password` ([ADR-0011](adr/0011-robot-password-secret-storage.md)).
- Robot description follows a stable key=value format the janitor parses to reverse-map robots → CRs ([ADR-0012](adr/0012-robot-description-as-component-contract.md)): `managed-by=harbor-workload-identity-bridge cluster=<X> harboraccess=<ns>/<name>`.
- Multi-cluster safety is two layers ([ADR-0009](adr/0009-multi-cluster-topology.md)): ownership-prefix on the name, AND cluster tag in the description (defense-in-depth against the known hyphen-prefix collision class).
- Janitor sweeps every 5 min, lists Harbor robots, parses descriptions, deletes orphans (no live CR).

**`bridge/dataplane`** — HTTPS server that accepts plugin requests, validates SA tokens, returns the robot's Basic Auth credentials ([ADR-0013](adr/0013-return-robot-basic-auth-credentials.md)):
- `oidc.go` — Validator wrapping `github.com/coreos/go-oidc/v3`. Cluster-bound issuer; per-request: signature, expiry, issuer. Audience and subject left to the handler.
- `handler.go` — POST `/v1/credentials`. Header `Authorization: Bearer <SA-token>`. Body `{"image": "..."}` (audit-only). Flow: gate → bearer extract → validate → list `HarborAccess` and match on (sub, aud) → read robot Secret → return `{username, password, expires_in, cache_key_type}`. Containerd does the registry auth handshake itself.
- **No JWT minting, no docker-token cache** — both were removed by ADR-0013. `harbor_token.go` and `cache.go` are gone; `github.com/jellydator/ttlcache/v3` is no longer a dep.

**`bridge/controlplane/harbor`** — SDK wrapper around `github.com/goharbor/go-client`. Used by the reconciler for `Create`/`Delete`/`List`/`GetByName`/`RefreshSecret`/`UpdatePermissions`. Robot name truncation logic. Unaffected by the ADR-0013 pivot.

## Conventions and constants reference

Read these before touching code in any phase.

### Env vars (bridge runtime)

| Var | Required | Notes |
| --- | --- | --- |
| `BRIDGE_CLUSTER_NAME` | yes | DNS-label regex `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars. Fail-fast on invalid. **Must not be a hyphen-prefix of any other cluster's name** sharing the same Harbor (operator responsibility per ADR-0009). |
| `BRIDGE_NAMESPACE` | yes | Where robot-password Secrets live. Same DNS-label validation. |
| `BRIDGE_OIDC_ISSUER` | yes | Cluster's service-account issuer, e.g. `https://kubernetes.default.svc`. Must match the `iss` claim of incoming SA tokens byte-for-byte. |
| `BRIDGE_HARBOR_URL` | yes | Base URL of the Harbor instance. |
| `BRIDGE_HARBOR_ADMIN_DIR` | yes | Directory holding `username` + `password` files (standard K8s Secret-as-volume mount). |
| `BRIDGE_FORCE_LOCAL_VALIDATION` | no | Default `true`. The `false` path returns 501 — reserved for post-Harbor-OIDC-migration. |
| `BRIDGE_LOG_LEVEL` | no | One of `debug`, `info`, `warn`, `error`. Default `info`. |

### Names and limits

- **Harbor robot username**: regex `^[a-z0-9]+(?:[._-][a-z0-9]+)*$`, postgres column `varchar(255)`. We use soft cap **240** in [bridge/controlplane/harbor/naming.go](../bridge/controlplane/harbor/naming.go). Overflows are deterministically hash-truncated.
- **Robot name format**: `bridge-<cluster>-<saNs>-<saName>`.
- **Secret name format**: `robot-<haNs>-<haName>` (bridge namespace). 253-char k8s limit; no truncation implemented yet — flagged as TODO in `reconciler.secretNameFor`.
- **Finalizer**: `harbor.aetherize.io/robot` ([controlplane/labels.go](../bridge/controlplane/labels.go)).
- **Managed-by label value**: `harbor-workload-identity-bridge`.

### Robot description (cross-component contract — ADR-0012)

Format: `managed-by=harbor-workload-identity-bridge cluster=<cluster> harboraccess=<haNs>/<haName>`

Evolution rule: additive only. Never reorder or remove existing tokens. The `managed-by=...` token is the version anchor that `RobotBelongsToCluster` checks first.

### Status conditions (HarborAccess)

`Ready` / `RobotProvisioned` / `TrustPolicyApplied`. Reasons used by the reconciler ([reconciler.go](../bridge/controlplane/reconciler.go)):

- `ReconcileSucceeded` — happy path Ready=True.
- `IssuerMismatch` — CR's `trustPolicy.issuer` ≠ bridge's `BRIDGE_OIDC_ISSUER`. Terminal (no retry).
- `RobotConflict` — existing Harbor robot at our name does not carry our cluster's description tag. Terminal.
- `InvalidSpec` — `RobotName` returned `ErrClusterNameTooLong` etc. Terminal.
- `HarborError` — transient Harbor failure; reconciler returns the error so controller-runtime retries with backoff.
- `EnforcedByBridge` — TrustPolicyApplied reason; status of bridge enforcement until #17520 lands.

The `markNotReady` vs `markTransientError` distinction is load-bearing for retry semantics — see [Phase 2 follow-up commit 2a73e08](https://github.com/...).

### Data plane HTTP API

```
POST /v1/credentials HTTP/1.1
Host: <bridge-service>
Authorization: Bearer <SA-token-with-aud-set-to-registry-hostname>
Content-Type: application/json

{"image": "harbor.example.com/production/myimg:v1"}  // body optional, audit-only
```

Response on success (200):

```json
{
  "username": "robot$bridge-<cluster>-<ns>-<sa>",
  "password": "<robot password from bridge-namespace Secret>",
  "expires_in": 3600,
  "cache_key_type": "ServiceAccount"
}
```

Status code map (post-pivot):
- `200` — credentials returned
- `400` — body is non-empty but malformed JSON
- `401` — missing/malformed Bearer header or invalid SA token (validator rejected)
- `403` — no `HarborAccess` matches the token's (sub, aud)
- `404` — wrong path
- `405` — non-POST
- `500` — internal error (unexpected k8s API failure, etc.)
- `501` — `forceLocalValidation=false` (alternative path reserved)
- `503` — robot Secret not yet present in bridge namespace; plugin should retry with backoff

Note that 502 is no longer used post-pivot (no /service/token call from the bridge means no upstream-failure path).

### KEP-4412 plugin-side cache type

The kubelet credential-provider config (installed by the Helm chart in Phase 5) must set `cacheType: ServiceAccount`. See [ADR-0006](adr/0006-oidc-validation-and-audience.md). The bridge response's `cache_key_type` field echoes this back to the plugin for forwarding.

## Phase 1 — Scaffolding + ADRs + CRD types — COMPLETE

ADRs 0001–0008, `HarborAccess` v1alpha1 types, generated manifests, Makefile, isolation lint.

## Phase 2 — Control Plane — COMPLETE

### Delivered

- ADR-0009 (multi-cluster topology) with prefix-collision operator caveat.
- ADR-0010 (`serviceAccountRef` as canonical identity; `subjectMatch` dropped in v1alpha1).
- ADRs 0011 + 0012 (Secret storage, description contract).
- [bridge/api/v1alpha1](../bridge/api/v1alpha1/) — `serviceAccountRef.{namespace,name}` required, DNS-1123 patterns; `tokenTTL` CEL bounded to 5m–24h.
- [bridge/controlplane/config.go](../bridge/controlplane/config.go) — fail-fast env loading with joined validation errors, K8s-Secret-as-volume admin-creds loader.
- [bridge/controlplane/harbor/](../bridge/controlplane/harbor/) — `Client` interface + go-client-backed impl; `RobotName`/`ClusterPrefix`/`OwnsRobot`/`IsValidHarborRobotName` pure helpers; client-side `FilterOwned`.
- [bridge/controlplane/reconciler.go](../bridge/controlplane/reconciler.go) — finalizer-based delete, two-layer adoption discipline, permission update on generation, password rotation on generation or 24h elapsed, status conditions. **Bug-fixed**: `markTransientError` triggers retry; `secretMissing` forces rotation.
- [bridge/controlplane/janitor.go](../bridge/controlplane/janitor.go) — `manager.Runnable`, 5-min default sweep, ownership + description filters.

### Known operator burden

- Cluster names must not be hyphen-prefixes of each other across bridges sharing one Harbor (ADR-0009 caveat).
- A `BRIDGE_CLUSTER_NAME` change orphans the prior `bridge-<oldname>-...` robots in Harbor; cleanup is manual.

## Phase 3 — Data Plane

### Slice A — OIDC Validator — COMPLETE

[bridge/dataplane/oidc.go](../bridge/dataplane/oidc.go) wraps `github.com/coreos/go-oidc/v3`. Constructor fails-fast on discovery. Per-request: signature, expiry, issuer (issuer hardcoded at construction). Audience and subject NOT checked here — handler does it with the matched CR in hand. Test fixture in [oidc_test.go](../bridge/dataplane/oidc_test.go) stands up an in-memory OIDC issuer (discovery + JWKS + RSA signer) and covers valid/expired/tampered/wrong-issuer/malformed paths.

### Slice B — Harbor /service/token + cache — REMOVED (ADR-0013 pivot)

Originally implemented, then deleted when ADR-0013 superseded ADR-0005. The data plane no longer mints JWTs because:

> Kubelet's `CredentialProviderResponse.Password` is consumed as HTTP Basic Auth by containerd. Containerd does the registry auth handshake itself (basic-auth POST to /service/token, receive scoped JWT, use JWT). A pre-minted JWT in `Password` would be sent as `Authorization: Basic base64(<token>:JWT)` to Harbor, which Harbor rejects.

This is preserved here so a future read of PHASES.md after compaction doesn't re-discover the deleted slice via git log and wonder why.

### Slice C — Credential-issuance handler — COMPLETE

[bridge/dataplane/handler.go](../bridge/dataplane/handler.go). Post-pivot: validate → match → read Secret → return Basic Auth credentials. ~200 LOC handler, 14 tests pinning every status-code path.

### Slice D — Server + metrics + cmd/main.go — COMPLETE

Delivered: [bridge/dataplane/server.go](../bridge/dataplane/server.go) (manager.Runnable HTTPS server with TLS-from-disk reloaded on every handshake, graceful shutdown bounded by 10s, optional mTLS via ClientCAFile), [bridge/dataplane/metrics.go](../bridge/dataplane/metrics.go) (Prometheus counters + histogram registered onto controller-runtime's `metrics.Registry` so /metrics serves both data-plane and reconciler metrics from one endpoint; OIDC error classifier with explicit `other` bucket for unknown go-oidc messages), handler.go wired with optional `*Metrics` (nil-safe for slim test fixtures), [bridge/cmd/main.go](../bridge/cmd/main.go) implementing the 9-step wiring. Integration-layer env vars (`BRIDGE_TLS_CERT_FILE`, `BRIDGE_TLS_KEY_FILE`, `BRIDGE_TLS_CLIENT_CA_FILE`, `BRIDGE_LISTEN_ADDR`, `BRIDGE_HEALTH_ADDR`, `BRIDGE_ENABLE_LEADER_ELECTION`) are read in main.go with sensible defaults; the controlplane-config layer is unchanged so its fail-fast joined-errors contract holds. `make build` produces `bin/bridge`; `make run-local` regenerates a 1-day self-signed cert and runs against `$KUBECONFIG`.

Tests: [server_test.go](../bridge/dataplane/server_test.go) covers happy-path serve + shutdown, busy-port bind error, mTLS rejecting clients without certificates, malformed CA bundle. [metrics_test.go](../bridge/dataplane/metrics_test.go) covers every label value of `bridge_credential_issuances_total`, the OIDC error classifier on all five reason buckets, double-increment when 503 fires (both `robot_secret_missing_total` and `issuances{result=unavailable}`), the nil-Metrics-still-works branch, and the Prometheus exposition format end-to-end.

Originally-planned section (kept for archaeology):

**Goal:** ship a runnable bridge binary. End-state: `BRIDGE_CLUSTER_NAME=… BRIDGE_HARBOR_URL=… … go run ./bridge/cmd` brings up the manager (reconciler + janitor) plus the HTTPS server (validator + handler) in one process.

**Files to add:**

1. `bridge/dataplane/server.go`
   - Wraps `http.Server` with TLS from disk and graceful shutdown.
   - `NewServer(cfg ServerConfig) (*Server, error)` returns something that implements `sigs.k8s.io/controller-runtime/pkg/manager.Runnable` so it can be added to the manager.
   - `ServerConfig`: ListenAddr (default `:8443`), CertFile, KeyFile, ClientCAFile (optional, enables mTLS — ADR-0008 mention), the assembled `http.Handler` (mux containing the credential handler + healthz + metrics endpoint).
   - On Start(ctx): start listener; on ctx.Done() perform `srv.Shutdown(timeout)` with a 10s timeout. Returning from Start signals manager shutdown.
   - TLS files reload? Defer to cert-manager handling (Phase 5) — cert-manager rotates the underlying Secret, the pod mounts via projected volume, cert change triggers a pod restart from cert-manager's renewBefore. Phase 5 may add `kubernetes-sigs/controller-runtime/pkg/certwatcher` for in-process reload if pod restarts are too disruptive.

2. `bridge/dataplane/metrics.go`
   - `github.com/prometheus/client_golang/prometheus` registry.
   - Counters (post-pivot, simpler than originally planned):
     - `bridge_credential_issuances_total{result="ok"|"forbidden"|"unauthorized"|"unavailable"}` — one increment per request, label by outcome.
     - `bridge_oidc_validation_failures_total{reason="expired"|"bad_signature"|"wrong_issuer"|"other"}` — increment in handler when validator returns error.
     - `bridge_harboraccess_lookup_failures_total` — k8s API failures during list.
     - `bridge_robot_secret_missing_total` — 503-path counter.
   - Histogram: `bridge_credential_issuance_duration_seconds` — per-request latency.
   - `metrics.Handler() http.Handler` exposes `/metrics` for the prometheus scraper. Lives behind the same TLS as the credential endpoint by default; chart can split if needed.
   - Refactor handler to take a `*Metrics` and `.Inc()` at each return site. Keep wiring minimal — no global state.

3. `bridge/cmd/main.go`
   - Package `main`. Single binary.
   - Step 1: load `controlplane.Config` from env. Log Sanitized() values (credentials excluded).
   - Step 2: build the scheme: `clientgoscheme` + `harborv1alpha1`.
   - Step 3: build `controller-runtime` Manager with options: Scheme, LeaderElection (true if replicas > 1; configured via env or flag — default off for Slice D, expose for Phase 5), Cache (default, can scope to specific namespaces later for perf), Metrics binding (off — we have our own).
   - Step 4: load admin creds via `cfg.LoadAdminCreds()`; build `harbor.Client` with them; pass to reconciler.
   - Step 5: instantiate `Reconciler` and call `SetupWithManager`.
   - Step 6: instantiate `Janitor` and add via `mgr.Add(janitor)`.
   - Step 7: instantiate `Validator` via `dataplane.NewValidator(ctx, dataplane.Config{Issuer: cfg.OIDCIssuer.String(), HTTPClient: …})`. Fail-fast on error.
   - Step 8: build `Handler`, build the HTTPS server, add server to manager via `mgr.Add(server)`.
   - Step 9: `mgr.Start(ctrl.SetupSignalHandler())`. Returns when SIGTERM/SIGINT arrives.
   - Logging: use `controller-runtime/pkg/log/zap` configured by `BRIDGE_LOG_LEVEL`. `ctrl.SetLogger(zap.New(...))` early so every component logs through one sink.

4. Update [Makefile](../Makefile) `build` target to produce `bin/bridge` from `./bridge/cmd`. Add `run-local` for development.

5. RBAC manifests (Phase 5 wires the chart, but the Reconciler's `+kubebuilder:rbac` markers already declare what's needed for the controlplane; add the data-plane equivalents):
   - `+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch` — data plane reads bridge-namespace Secrets.
   - `+kubebuilder:rbac:groups=harbor.aetherize.io,resources=harboraccesses,verbs=get;list;watch` — data plane lists CRs to route requests.
   - Plus the existing controlplane RBAC. `make manifests` regenerates `config/rbac/`.

**Tests:**

- `server_test.go` — start the server on a random port (`:0`), POST a request, verify response. Cert generation in-test via `crypto/tls` self-signed.
- `metrics_test.go` — increment each counter via the handler, scrape `/metrics`, assert presence of expected metric families.
- main.go is exercised by Phase 6 e2e — no Go-level unit test for it.

**Done criteria:**

- `go run ./bridge/cmd` starts and serves on `:8443` against a local kind cluster + local Harbor.
- `curl -k --cacert ca.crt -H "Authorization: Bearer $TOKEN" https://localhost:8443/v1/credentials -d '{"image":"…"}'` returns credentials.
- `/metrics` reports the expected metric families.
- SIGTERM cleanly shuts down both the reconciler and the HTTP server.

## Phase 4 — Plugin binary — COMPLETE

### Delivered

- [plugin/main.go](../plugin/main.go) — entrypoint, env-loading (`HARBOR_BRIDGE_ENDPOINT`, `_CA_BUNDLE`, `_CLIENT_CERT`, `_CLIENT_KEY`), stdin/stdout protocol (`credentialprovider.kubelet.k8s.io/v1`), translation of bridge response → `CredentialProviderResponse`. Image-host derivation preserves the port (e.g. `harbor.example.com:8443`) so the auth-map key matches kubelet's image-to-auth lookup exactly. Wire types are defined locally instead of imported from `k8s.io/kubelet/pkg/apis/credentialprovider/v1` — keeps the binary's transitive dep graph free of `k8s.io/api`/controller-runtime and dodges the upstream `PluginCacheKeyType` enum mismatch around the bridge's `cache_key_type="ServiceAccount"` value.
- [plugin/bridge_client.go](../plugin/bridge_client.go) — HTTPS-only client (`https://` scheme enforced in config validation). 5s connect timeout, 15s total. TLS verification via `HARBOR_BRIDGE_CA_BUNDLE` (path *or* inline PEM, detected by `-----BEGIN` armor). Optional mTLS via `_CLIENT_CERT`/`_CLIENT_KEY` (both-or-neither, validated). 503 → one retry after 1s; second 503 surfaces as `errBridgeUnavailable` (non-zero exit). 401/403 → `errBridgeRefused` sentinel that the main loop translates into an empty-auth response (no kubelet caching).
- Exit-code contract: 0 on success or on a successful empty-auth refusal write; non-zero on any other error with the cause on stderr (kubelet surfaces stderr in node events).
- Plugin does **not** import `bridge/dataplane` — the `/v1/credentials` path and `bridgeResponse` shape are duplicated with "must match" comments. Verified by `go list -deps ./plugin/...` showing zero `k8s.io` / `sigs.k8s.io` packages in the closure.
- Build: `make build-plugin` → `bin/harbor-bridge-plugin` (~6 MB static, `CGO_ENABLED=0`, `-trimpath -ldflags='-s -w'`). Phase 5 distroless image is `gcr.io/distroless/static:nonroot`.

### Tests

- [plugin/main_test.go](../plugin/main_test.go) — `run()` end-to-end through a `bridgeFetcher` fake (happy path, host-with-port retention, refused→empty auth, unavailable→error propagation, JSON decode failure, missing fields), plus `imageHost` table and `loadConfig` matrix (missing endpoint, http rejected, mTLS partial set).
- [plugin/bridge_client_test.go](../plugin/bridge_client_test.go) — every status-code path against `httptest.NewTLSServer`: 200 (with request-header/body assertions), 401, 403, 503→200 retry (with elapsed-time check), 503→503 mapping, 500 not retried, 200-with-empty-creds, 200-with-garbage-body, unreachable-server.

### Originally-planned section (kept for archaeology)

**Goal:** static Go binary the kubelet executes per KEP-4412.

### KEP-4412 essentials to encode

- Read `CredentialProviderRequest` JSON from stdin. Fields: `apiVersion: credentialprovider.kubelet.k8s.io/v1`, `kind: CredentialProviderRequest`, `image`, `serviceAccountToken`.
- Write `CredentialProviderResponse` JSON to stdout. Fields: `cacheKeyType: "ServiceAccount"`, `cacheDuration: <metav1.Duration>`, `auth: {<host>: {username, password}}`.
- Exit code 0 on success, non-zero on failure (kubelet treats stderr as the error message).
- Beta in 1.34: `cacheType` field is required in the credential-provider config (chart concern, not plugin code).

### Plugin behaviour

- Read env: `HARBOR_BRIDGE_ENDPOINT` (required), `HARBOR_BRIDGE_CA_BUNDLE` (path or PEM inline), optional `HARBOR_BRIDGE_CLIENT_CERT`/`_KEY` for mTLS (ADR-0008).
- POST `{image: req.image}` to `<endpoint>/v1/credentials` with `Authorization: Bearer <req.serviceAccountToken>`.
- 5s connect timeout, 15s total request timeout.
- On 200: read body, translate bridge `Response` into `CredentialProviderResponse`. Use `cache_key_type` for `cacheKeyType`, `expires_in` (seconds) for `cacheDuration`. The single `auth` entry's host comes from the image's host portion.
- On 401/403: write empty `auth` map and `cacheKeyType: Image`, `cacheDuration: 0s` — instructs kubelet NOT to cache, retry on each pull, plus emit the bridge's error to stderr for kubelet to surface.
- On 503: retry once after 1s; if still 503, fail (kubelet retries via its own backoff).
- On 5xx: fail; kubelet retries on next image-pull attempt.

### File layout

- `plugin/main.go` — entrypoint, env load, stdin/stdout protocol.
- `plugin/bridge_client.go` — small HTTP client.
- Tests: `main_test.go` against a fake bridge (httptest); cover all status-code paths and stdin/stdout round-trip.

### Build

- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64,arm64 go build -ldflags='-s -w' -o bin/harbor-bridge-plugin ./plugin`.
- Distroless `gcr.io/distroless/static:nonroot` image, single layer with the binary.
- Helm chart (Phase 5) deploys as a DaemonSet whose only job is to copy the binary onto the node's `/etc/kubernetes/credential-provider/` directory plus the kubelet credential-provider config to `/etc/kubernetes/credential-provider-config/`.

## Phase 5 — Helm chart — COMPLETE

### Delivered

- [charts/harbor-bridge/Chart.yaml](../chart/Chart.yaml) (`harbor-workload-identity-bridge` v0.1.0, kubeVersion `>=1.34.0-0` for KEP-4412 beta).
- [charts/harbor-bridge/values.yaml](../chart/values.yaml) — 7 REQUIRED fields gated at template time (`clusterName`, `harbor.url`, `harbor.adminCredsSecret.name`, `plugin.matchImages`, `plugin.audience`, `tls.issuerRef.name` when `tls.enabled`, `bridge.mTLS.clientIssuerRef.name` when mTLS enabled); everything else defaulted with comments explaining each knob.
- [charts/harbor-bridge/templates/_helpers.tpl](../chart/templates/_helpers.tpl) — `validateRequiredValues` (`fail` with action-oriented messages), component-scoped names (`harbor-bridge` for the bridge, `harbor-bridge-plugin` for the daemon), `clusterScopedName` for ClusterRole/Binding so two installs in different namespaces don't collide, leader-election auto-derivation from replica count, and image-tag fallback to `.Chart.AppVersion`.
- Bridge templates: `bridge-serviceaccount.yaml`, `bridge-rbac.yaml` (ClusterRole for HarborAccess cluster-wide, Role for Secrets + Lease in release namespace — tightened from the over-broad kubebuilder markers), `bridge-deployment.yaml` (2 replicas with pod-anti-affinity, distroless `nonroot` security context, projected admin-creds and TLS volumes, TLS-cert-checksum annotation for cert-rotation reload), `bridge-service.yaml` (NodePort 31443 per ADR-0008), `bridge-certificate.yaml` (cert-manager Certificate with optional client-cert when mTLS enabled), `bridge-servicemonitor.yaml` (optional, only when `metrics.serviceMonitor.enabled`).
- Plugin templates: `plugin-serviceaccount.yaml` (`automountServiceAccountToken: false` — plugin pods don't talk to the K8s API), `plugin-configmap.yaml` (kubelet `CredentialProviderConfig` with `cacheType: ServiceAccount`, `defaultCacheDuration`, `matchImages` from values, `HARBOR_BRIDGE_*` env including CA path), `plugin-daemonset.yaml` (privileged init container that copies binary + config + CA + optional mTLS client cert into `/etc/kubernetes/credential-provider*` hostPaths, then a `registry.k8s.io/pause:3.10` main container so the DaemonSet stays "running" and `kubectl logs` surfaces the install output; `priorityClassName: system-node-critical`, `tolerations: [{operator: Exists}]` so the plugin lands on control-plane nodes).
- [charts/harbor-bridge/crds/harbor.aetherize.io_harboraccesses.yaml](../chart/crds/harbor.aetherize.io_harboraccesses.yaml) — CRD copied from `config/crd/bases`. Helm's `crds/` directory installs it but does not upgrade it; CRD changes are an explicit operator step.
- [charts/harbor-bridge/templates/NOTES.txt](../chart/templates/NOTES.txt) — prints `clusterName`, the unique prefix, the audience the operator must set on every CR, and the kubelet flags the chart cannot set (`--image-credential-provider-{bin-dir,config}` must already be on the node).

### Validation

- `make chart-lint` — `helm lint` clean on both test values files (`values-complete.yaml`, `values-mtls.yaml`).
- `make chart-test-required` — [charts/harbor-bridge/tests/test-required-values.sh](../chart/tests/test-required-values.sh) verifies all 7 required-value gates fire with the expected error substring. `plugin.matchImages` (an empty list, not a missing key) is tested via a values overlay because `--set foo=[]` doesn't reproduce the empty-list path.
- `make chart-golden` — diff current render against `charts/harbor-bridge/tests/golden/{default,mtls}.yaml`. CI fails on unintended template drift; `make chart-golden-update` re-captures after intentional changes.
- `make chart-test` — full suite (lint + required-value + golden), used as the chart's gate.
- Server-side dry-run against the 2026-05-31 kind cluster accepted all 11 resources for the default variant and all 12 for the mTLS variant (the ServiceMonitor's CRD wasn't installed; YAML structurally fine).
- [Makefile](../Makefile) gains `verify-plugin-isolation` which fails the build if `go list -deps ./plugin/...` returns any `k8s.io` / `sigs.k8s.io` package — mechanises ADR-0015's consequence.

### Operator-facing notes

- `tls.issuerRef` points at any cert-manager Issuer/ClusterIssuer the operator has. The Certificate writes `ca.crt` into the bridge-tls Secret; the plugin DaemonSet mounts that Secret and copies the CA onto each node, so kubelet's plugin process can verify the bridge's TLS cert.
- mTLS adds a second Certificate (`<release>-plugin-mtls-client`) and threads `HARBOR_BRIDGE_CLIENT_CERT` / `_KEY` into the kubelet config. Bridge's `BRIDGE_TLS_CLIENT_CA_FILE` is enabled at the same time, rejecting plugin connections without a client cert.
- The plugin DaemonSet **cannot** set kubelet's `--image-credential-provider-*` flags. The chart's NOTES.txt prints the required values; operators set them at node provisioning.

### Originally-planned section (kept for archaeology)

**Goal:** one chart deploys the whole system; install fails clearly when misconfigured.

### Required values

- `clusterName` — REQUIRED via `required` helper. DNS-label validated. Plumbed to bridge as `BRIDGE_CLUSTER_NAME`.
- `harbor.url` — required.
- `harbor.adminCredsSecret` — required, references a Secret in the chart's install namespace with `username` + `password` keys.

### Defaulted values

- `forceLocalValidation: true`
- `nodePort: 31443` (ADR-0008)
- `bridge.replicas: 2`
- `bridge.image`, `plugin.image`, `bridge.pullPolicy`
- `tls.enabled: true`, `tls.certManagerIssuer: …` (Issuer reference)
- `tls.mTLS.enabled: false` (mTLS optional; chart provisions client cert when on)
- `logLevel: info`
- `metrics.enabled: true` — exposes `/metrics` from the bridge Service

### Templates

- `bridge-deployment.yaml` — Deployment, `BRIDGE_*` env, projected volumes for: admin creds Secret (mounted at `BRIDGE_HARBOR_ADMIN_DIR`), TLS cert (mounted at `/etc/bridge/tls`), the cert-manager-managed CA.
- `bridge-service.yaml` — NodePort, port 8443 → nodePort 31443 (or dynamic).
- `bridge-certificate.yaml` — cert-manager `Certificate` resource targeting the bridge Service DNS.
- `bridge-rbac.yaml` — ClusterRole + ClusterRoleBinding for `system:service-account-issuer-discovery`; Role + RoleBinding in the bridge namespace for Secret get/list/watch; ClusterRole + Binding for `harboraccesses.harbor.aetherize.io` get/list/watch + finalizers + status.
- `plugin-daemonset.yaml` — DaemonSet that mounts host paths and copies the plugin binary + credential-provider config on container start.
- `plugin-configmap.yaml` — kubelet credential-provider config: `cacheType: ServiceAccount`, matchImages list, `defaultCacheDuration`, env to point plugin at the bridge endpoint.
- `crds/` — CRDs copied from `config/crd/bases/`.
- `NOTES.txt` — prints configured `clusterName` and "remember to use a unique name across clusters that share this Harbor" warning.

### Validation

- `helm chart-testing` (`ct lint`) clean.
- Golden-file render test: render with sample `values.yaml`, diff against `tests/golden/`.
- Required-value test: `helm template` without `clusterName` must fail.

## Phase 6 — kubelet-driven e2e + finalize docs — IN PROGRESS

**Scope change from original plan:** the two-cluster e2e (Cluster A `prod-eu-west` + Cluster B `staging-us-east` against one Harbor) is dropped. Single-cluster verification already covers the architectural claims, the multi-cluster ownership model is mechanically verified by [ADR-0009](adr/0009-multi-cluster-topology.md) + the prefix-check unit tests, and standing up two real kind clusters with cert-manager + Harbor on each is disproportionate setup for the marginal coverage it adds.

**Goal:** kubelet-driven pull works end-to-end on a single kind cluster, configured for KEP-4412 with the chart-installed plugin.

### What was verified during the 2026-05-31 single-cluster run

Everything below works on the `tofu-dev` kind cluster (k8s 1.35, 1 cp + 3 workers, cert-manager + Harbor in-cluster) with the chart installed via `helm install harbor-bridge ./charts/harbor-bridge -n harbor-bridge-system -f charts/harbor-bridge/tests/values-e2e.yaml`:

- Chart installs cleanly; bridge Deployment + plugin DaemonSet both Ready.
- Bridge → apiserver TLS + auth works in-cluster via `BRIDGE_OIDC_CA_FILE` + per-request Bearer-token RoundTripper (commit `7615794` fixed three real bugs hidden by yesterday's laptop-bridge setup).
- HarborAccess CR reaches `Ready=True`; robot `bridge-dev-test-pull-image-puller` appears in Harbor; per-CR Secret created in bridge namespace.
- Plugin binary installed on every node at `/etc/kubernetes/credential-provider/harbor-bridge-plugin` by the DaemonSet, with config + CA at `/etc/kubernetes/credential-provider-config/`.
- Direct invocation of the on-node binary via `docker exec -i <node> ... harbor-bridge-plugin` (with kubelet-equivalent stdin and env) returns a `CredentialProviderResponse` with a Basic Auth pair **byte-equal** to what `curl /v1/credentials` returns through `kubectl port-forward`.
- After patching the four kubelet configs (`/etc/default/kubelet` with `KUBELET_EXTRA_ARGS="--image-credential-provider-bin-dir=... --image-credential-provider-config=..."` — the KubeletConfiguration v1beta1 schema does **not** accept `imageCredentialProvider*` fields in this kubelet build, despite the docs implying it should; `strict decoding error: unknown field` confirms), kubelet:
  - Registers the provider at startup: `plugins.go:55 "Registered credential provider" provider="harbor-bridge-plugin"`.
  - Matches the provider when scheduling the test pod: `plugins.go:75 "Generating per pod credential provider" provider="harbor-bridge-plugin" podName="pull-test" podNamespace="test-pull" podUID="…" serviceAccountName="image-puller"`.

### Where it stops — the open blocker

Between the `Generating per pod credential provider` log and `Pulling image without credentials` ~2 ms later, kubelet **silently aborts**. The plugin binary is never executed.

**Definitive proof:** the plugin binary was replaced with a wrapper script that logs every invocation to `/tmp/plugin-invoked.log` before exec'ing the real binary. The log file is never created — kubelet does not exec the binary at all.

**What we verified is NOT the cause:**

- Plugin works when invoked manually (`docker exec -i tofu-dev-worker ... harbor-bridge-plugin`). Returns valid response.
- Bridge serves `/v1/credentials` correctly (verified via curl through port-forward, with byte-equal Basic Auth to direct plugin invocation).
- Kubelet command line shows the flags correctly: `--image-credential-provider-bin-dir=/etc/kubernetes/credential-provider --image-credential-provider-config=/etc/kubernetes/credential-provider-config/credential-provider-config.yaml`.
- The credential-provider-config.yaml file on the node has correct content; `tokenAttributes.serviceAccountTokenAudience` is `harbor-bridge`, `cacheType` is `ServiceAccount`.
- `kubectl create token image-puller -n test-pull --audience=harbor-bridge --duration=10m` succeeds — apiserver accepts the audience.
- Bridge metrics show **0** issuances during the kubelet-pull window (the plugin was never invoked).
- Kubelet at `--v=4` and `--v=6` shows no `Error getting service account token`, no `RequestServiceAccountToken`, no plugin exec attempt; the next log line after `Generating per pod credential provider` is `BackOff` or the next sync — nothing between.

**Three plausible causes, in order of likelihood:**

1. **KEP-4412 ServiceAccount-token-for-credential-providers feature gate.** The relevant gate is `KubeletServiceAccountTokenForCredentialProviders` (beta in 1.34, default-enabled per upstream). Kind's 1.35 build may compile or configure it differently. Quick check: omit `tokenAttributes` entirely (use the older Image-cache-type provider path) and see if kubelet exec's the binary. If the binary runs without `tokenAttributes` but not with, the gate or its expected wiring is the cause.
2. **Token projection failing silently.** Kubelet calls `TokenRequest` on the apiserver to mint a pod-bound SA token. If that call 401s/403s, kubelet returns empty credentials without logging at V=6. `kubectl create token` succeeds for the same SA + audience, but `kubectl` uses an unbound token whereas kubelet projects a pod-bound one — different code path on the apiserver. Quick check: run kubelet with `--v=8` and grep for `tokenmanager`, `RequestServiceAccountToken`, `TokenRequest`.
3. **`tokenAttributes` schema mismatch in this kubelet's v1 CredentialProviderConfig.** The chart's config file uses `apiVersion: kubelet.config.k8s.io/v1`. If this kubelet's compiled-in v1 schema doesn't include `tokenAttributes` (was promoted later or only in v1beta1), kubelet may parse the provider, register it, log "Generating", then bail when actually wiring the token attributes. Quick check: change the config's `apiVersion` to `kubelet.config.k8s.io/v1beta1`.

### Recommended pick-up

Before further chart or plugin changes, run the three quick checks above in order. Each is 5 min and one will narrow the cause. Most likely outcome: a chart-side fix (drop `tokenAttributes` for this k8s version, or downshift the CredentialProviderConfig apiVersion).

### Cluster state left behind (as of 2026-05-31)

- Bridge + plugin DaemonSet installed in `harbor-bridge-system` namespace.
- HarborAccess CR `test-access` applied; `Ready=True`.
- ClusterIssuer `harbor-bridge-ca` (self-signed) and `harbor-admin` Secret in place.
- All 4 kubelet configs have `--image-credential-provider-*` flags via `/etc/default/kubelet`; kubelet running.
- KubeletConfiguration files restored to original (the `imageCredentialProvider*` fields were rejected as unknown — flags-only is the working path).
- Plugin binary restored on `tofu-dev-worker` (debug wrapper removed).

### 2026-06-03 — kubelet startup check + DaemonSet self-installs the kubelet flags

Two new discoveries from the 2026-06-03 attempt to run the cluster with the kubelet flags pre-baked via terraform:

1. **Kubelet refuses to start when either `--image-credential-provider-bin-dir` or `--image-credential-provider-config` points at a non-existent path.** The check is in `pkg/kubelet/kuberuntime/kuberuntime_manager.go:301`; failure emits `Failed to register CRI auth plugins err="plugin binary directory ... did not exist"` / `unable to access path ".../credential-provider-config.yaml": no such file or directory`. kubelet then crash-loops, kubeadm init fails, terraform-kind fails to create the cluster. This is also the most likely retro-explanation for the silent-abort behaviour seen on 2026-05-31: kubelet was patched on a *running* cluster where the files were already on disk, but on a *fresh* boot the same flags would have prevented kubelet from coming up at all.
2. **There is no upstream pattern for installing credential providers via a Kubernetes workload.** ECR, GCR, and ACR all bake the binary + config + kubelet flags into the cloud provider node image; the chart isn't running on those nodes. For self-managed clusters (kind, vanilla kubeadm, k3s) the standard advice is to place files at node-provisioning time (cloud-init / Ansible / Terraform `remote-exec`). No widely-used "install via DaemonSet" pattern exists.

**The chart now closes the gap itself.** [charts/harbor-bridge/templates/plugin-daemonset.yaml](../chart/templates/plugin-daemonset.yaml) gained `hostPID: true` and an `nsenter`-into-PID-1 block that, after copying the binary + config + CA into place, writes `KUBELET_EXTRA_ARGS` into `/etc/default/kubelet` and runs `systemctl daemon-reload && systemctl restart kubelet` — once per node, guarded by an idempotency `grep`. Gated on `plugin.patchKubelet: true` (default; flip to false when the node image already wires kubelet, e.g. EKS).

**Test-cluster terraform module reverted.** [modules/test-cluster/main.tf](https://github.com/50hz/terraform-modules) keeps the feature gates (`KubeletServiceAccountTokenForCredentialProviders`, `ServiceAccountNodeAudienceRestriction`) but no longer sets `--image-credential-provider-*` flags itself — the chart owns kubelet wiring end-to-end.

### 2026-06-04 — fully automated reproduction in `test/e2e/`

The blocker is now reproducible from a clean repo via a single `tofu test` invocation in [test/e2e/](../test/e2e/). The test:

1. `cluster` — kind v1.35.0, Cilium kube-proxy replacement, cert-manager, containerd `hosts.toml` skip-verify for the Harbor NodePort, `kind load` of the locally-built bridge + plugin images.
2. `harbor` — Harbor via the upstream helm chart, exposed as NodePort 30843 with auto self-signed cert.
3. `seed_image` — in-cluster Job that creates the `your-project` Harbor project and `crane copy`s `alpine:3.20` into it.
4. `bridge_install` — our chart, including the plugin DaemonSet's `nsenter` patch-and-restart of kubelet on every node.
5. `harbor_access` — HarborAccess CR + test SA.
6. `pull_pod` — Job pulling `127.0.0.1:30843/your-project/alpine:test3`.

**Outcome (2026-06-04 run):** 5 of 6 pass. `pull_pod` fails. `test/e2e/diag-last/` captures, from the *live* cluster moments before tofu's destroy phase:

- `bridge-metrics.txt` — every `bridge_credential_issuances_total{result=*}` counter is **0**. The bridge was never reached.
- `pull-test.describe.txt` — pod `ErrImagePull`, `401 Unauthorized`. Containerd attempted the pull without credentials.
- `bridge-e2e-worker.node-state.log` — `/etc/default/kubelet` carries both `--image-credential-provider-*` flags, the binary exists at `/etc/kubernetes/credential-provider/harbor-bridge-plugin` (mode 0755), the config file's `matchImages` is `127.0.0.1:30843/*` which exactly matches the test image reference.

All preconditions kubelet needs to invoke the plugin are met. Kubelet still doesn't. This is the silent-abort blocker, **now reproduced in a fully scripted, clean-room environment** rather than on a manually-poked tofu-dev cluster.

[docs/UPSTREAM-ISSUE.md](UPSTREAM-ISSUE.md) is the ready-to-file draft; the embedded `kind create cluster` + minimal printer-plugin recipe still stands and is independent of our chart.

### Remaining work

1. **File the upstream issue** — kubernetes/kubernetes. The standalone reproduction in `UPSTREAM-ISSUE.md` is a minimum-viable kind-only repro that the maintainers can run in seconds.
2. **`02-traefik.tftest.hcl`** — production-realistic test that adds Traefik + IngressRoute on top of `01-bridge.tftest.hcl`. Separate test so the fast-feedback `01` stays under 15 minutes. Today the e2e drops Traefik and uses NodePort + `skip_verify` on containerd, which is enough to prove the bridge / plugin / chart wiring but does not cover the Traefik path operators will deploy.
3. **`docs/SECURITY.md` polish.** Needs a pass to reflect the elevated privilege the install DaemonSet now requires (`hostPID: true`, `privileged: true`, restarts kubelet on every node).
4. **`docs/ARCHITECTURE.md`.** Optional for v0.1.0.
5. **`v0.1.0` tag.** Annotated tag, push, release notes summarising Phases 1–6 + the upstream blocker.

### Originally-planned section (kept for archaeology)

**Goal:** two kind clusters share one Harbor; each manages only its own robots; image pulls succeed.

The original two-cluster setup is preserved below in case we revisit it for a multi-cluster CI gate. Most claims it would validate are now mechanically covered by unit tests + the 2026-05-31 single-cluster run.

#### e2e setup

- `test/e2e/` with `sigs.k8s.io/e2e-framework`.
- Two kind clusters: `prod-eu-west` and `staging-us-east`. Different `clusterName` values.
- One Harbor: docker-compose or kind-deployed. Pre-create projects `production` + `shared-base-images`.
- Apply `HarborAccess` in each cluster (sample CR).
- Deploy a test pod that image-pulls from Harbor; assert pull succeeds within timeout.

#### What the e2e validates

- ADR-0013's claim: containerd accepts our Basic Auth credentials and successfully completes its registry handshake with Harbor. **Already proven via crane on 2026-05-31.**
- ADR-0009's claim: cluster A's robots have prefix `bridge-prod-eu-west-…`, cluster B's have `bridge-staging-us-east-…`. Audit Harbor robot list and assert no cross-contamination. **Covered by `harbor.OwnsRobot` unit tests and the prefix-construction logic in `harbor.RobotName`; a real two-cluster run would be confirmation rather than discovery.**
- ADR-0011's claim: workloads in cluster A cannot read the robot Secret (RBAC test). **Single-cluster RBAC test would suffice; the chart's Role/RoleBinding scopes Secrets to the bridge namespace only.**
- ADR-0012's claim: deleting a `HarborAccess` causes the janitor to remove the Harbor robot. **Single-cluster test, no need for two.**

#### Docs to write

- `docs/ARCHITECTURE.md` — request flow diagram (sequence-diagram-as-mermaid), component overview, lifecycle of a HarborAccess.
- `docs/SECURITY.md` — threat model, blast-radius analysis post-ADR-0013, mTLS guidance, audit-log shape, SA-revocation latency.
- `README.md` — install walkthrough, two-cluster example, troubleshooting (incl. the prefix-collision caveat). **Rewritten 2026-05-31 to chart-led quickstart with single-cluster scope.**
- `docs/MIGRATION.md` — extend with post-pivot decommission walkthrough and the `forceLocalValidation` flip story for the Harbor #17520 future. **Stays a stub until #17520 has a public direction.**

#### Tag

`v0.1.0` after kubelet-exec blocker resolved + verification green + SECURITY.md polished.

## Cross-cutting non-goals (intentionally not in any phase)

- Central Bridge or coordinator across clusters.
- Distributed locking or shared state between bridges.
- Per-cluster identity carried inside the CRD.
- Automatic `clusterName` detection from kubeconfig context or labels.
- One-cluster-to-N-Harbors topology.
- Cross-cluster robot deduplication.
- Pre-mint Docker JWTs from the bridge (was ADR-0005, superseded by ADR-0013).
- The bridge serving as a Docker registry token endpoint itself (incompatible with how Harbor configures its registry auth realm).

## Known issues and future hardening (post-v0.1.0)

- **Secret-name truncation**: `secretNameFor` in [reconciler.go](../bridge/controlplane/reconciler.go) does not hash-truncate. CRs with very long combined namespace+name could overflow Kubernetes' 253-char limit. Mirror the hash-truncate from `harbor/naming.go` when this becomes a real issue.
- **Reconciler doesn't gate on CR Ready state in data plane**: a CR in `Ready=False` (e.g. issuer mismatch) will surface to the data plane only via "Secret missing → 503" loop. The plugin retries indefinitely. Optional hardening: data plane checks the CR's Ready condition and returns 403 if not Ready, with the reason in the body.
- **Permission-edit blip**: between a `spec.permissions` edit and the next reconcile, the data plane could mint credentials for permissions the in-Harbor robot doesn't yet have. Harbor's `/service/token` will issue a JWT that doesn't include the not-yet-granted scope; containerd's pull fails with 403 until reconcile catches up. Operator-perceptible blip; acceptable.
- **Janitor at scale**: lists all Harbor robots on each sweep. O(robots) per 5min. Fine for hundreds, marginal for thousands; consider Harbor query-param filter if it becomes a problem.
- **CRD validation tests**: the CRD CEL/pattern markers are not round-tripped through a real apiserver. Add envtest-based validation tests in Phase 6 polish.
- **`controlplane.Config.LoadAdminCreds` reload**: credentials load once at startup. If admin creds rotate, the bridge needs a restart. cert-manager pattern (pod restarts on Secret change) covers this — chart concern.

## Open questions

| Topic | Resolution path |
| --- | --- |
| Does containerd's auth flow accept our Basic Auth credentials end-to-end? | **Resolved 2026-05-31.** `crane pull` (uses the same Bearer-token handshake as containerd) succeeded against Harbor 2.x with credentials returned by the bridge; full procedure and captured output in [HOW-TO-TEST.md](../HOW-TO-TEST.md). Phase 6 e2e will re-validate against real containerd in kind. |
| Should the data plane gate on CR `Ready=True` before returning credentials? | Phase 6 polish. Trade-off: stronger guarantee vs more code in the hot path. |
| Should `forceLocalValidation: false` ever default `true`? | Reassess when Harbor #17520 lands. Air-gapped clusters keep `true` indefinitely. |
| Should the chart split metrics onto a separate port? | Phase 5 design call. Currently planned: same port, same TLS. |
