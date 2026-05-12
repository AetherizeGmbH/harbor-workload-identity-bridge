# Project phases

Source of truth for what is done, what is next, and what is intentionally out of scope. Each phase ends with a working, tested deliverable. Cross-phase work is rejected — push to a later phase.

This document is written to survive context compaction. The detail sections below carry the load-bearing constraints, conventions, and constants — read them before resuming work in a phase.

## Current status (as of 2026-05-12)

- Phase 1: COMPLETE
- Phase 2 Slice A (CRD bump + control-plane foundations): COMPLETE
- Phase 2 Slice B (reconciler + janitor): COMPLETE
- Phase 2 follow-up (transient-retry + Secret-rebuild fixes): COMPLETE
- Phase 3 Slice A (OIDC validator): COMPLETE
- Phase 3 Slice B (Harbor token client + cache): **REMOVED in ADR-0013 pivot** — see "Architecture snapshot" below
- Phase 3 Slice C (handler): COMPLETE (rewritten by ADR-0013 pivot)
- Phase 3 Slice D (server + metrics + cmd/main.go): **NEXT**
- Phase 4 (plugin binary): pending
- Phase 5 (Helm chart): pending
- Phase 6 (e2e + docs): pending

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

### Slice D — Server + metrics + cmd/main.go — NEXT

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

## Phase 4 — Plugin binary

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

## Phase 5 — Helm chart

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

## Phase 6 — e2e + finalize docs

**Goal:** two kind clusters share one Harbor; each manages only its own robots; image pulls succeed.

### e2e setup

- `test/e2e/` with `sigs.k8s.io/e2e-framework`.
- Two kind clusters: `prod-eu-west` and `staging-us-east`. Different `clusterName` values.
- One Harbor: docker-compose or kind-deployed. Pre-create projects `production` + `shared-base-images`.
- Apply `HarborAccess` in each cluster (sample CR).
- Deploy a test pod that image-pulls from Harbor; assert pull succeeds within timeout.

### What the e2e validates

- ADR-0013's claim: containerd accepts our Basic Auth credentials and successfully completes its registry handshake with Harbor.
- ADR-0009's claim: cluster A's robots have prefix `bridge-prod-eu-west-…`, cluster B's have `bridge-staging-us-east-…`. Audit Harbor robot list and assert no cross-contamination.
- ADR-0011's claim: workloads in cluster A cannot read the robot Secret (RBAC test: deploy a pod with default SA, `kubectl exec` into it, try to read the Secret, expect Forbidden).
- ADR-0012's claim: deleting a `HarborAccess` causes the janitor to remove the Harbor robot. Tweak janitor sweep interval to 5s in e2e for tractable test runtime.

### Docs to write

- `docs/ARCHITECTURE.md` — request flow diagram (sequence-diagram-as-mermaid), component overview, lifecycle of a HarborAccess.
- `docs/SECURITY.md` — threat model, blast-radius analysis post-ADR-0013, mTLS guidance, audit-log shape, SA-revocation latency.
- `README.md` — install walkthrough, two-cluster example, troubleshooting (incl. the prefix-collision caveat).
- `docs/MIGRATION.md` — extend with post-pivot decommission walkthrough and the `forceLocalValidation` flip story for the Harbor #17520 future.

### Tag

`v0.1.0` after e2e green + docs reviewed.

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
| Does containerd's auth flow accept our Basic Auth credentials end-to-end? | Phase 6 e2e. ADR-0013 commits to this; if e2e disproves it, the architecture has to change again. |
| Should the data plane gate on CR `Ready=True` before returning credentials? | Phase 6 polish. Trade-off: stronger guarantee vs more code in the hot path. |
| Should `forceLocalValidation: false` ever default `true`? | Reassess when Harbor #17520 lands. Air-gapped clusters keep `true` indefinitely. |
| Should the chart split metrics onto a separate port? | Phase 5 design call. Currently planned: same port, same TLS. |
