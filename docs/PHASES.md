# Project phases

Source of truth for what is done, what is next, and what is intentionally out of scope.
Each phase ends with a working, tested deliverable. Cross-phase work is rejected — push to a later phase.

## Phase 1 — Scaffolding + ADRs + CRD types — **COMPLETE**

- ADRs 0001–0008 written (`docs/adr/`).
- `HarborAccess` v1alpha1 types ([bridge/api/v1alpha1/](../bridge/api/v1alpha1/)).
- Generated `zz_generated.deepcopy.go` and `config/crd/bases/harbor.aetherize.io_harboraccesses.yaml`.
- `Makefile` targets: `generate`, `manifests`, `tidy`, `fmt`, `vet`, `build`, `test`, `verify-package-isolation`.
- `go build ./...`, `go vet ./...`, `gofmt -l .` all clean.

## Phase 2 — Control Plane, multi-cluster from day one — **NEXT**

Goal: Reconciler that turns a `HarborAccess` CR into a persistent Harbor robot, strictly scoped to its own cluster via `clusterName`. Built per [docs/multi-cluster spec][mc-spec] from the start; no retrofit.

[mc-spec]: ./adr/0009-multi-cluster-topology.md

### ADR
- **ADR-0009: Multi-Cluster Topology** — written before any Phase-2 code. Covers bridge-per-cluster, ownership-filter as safety invariant, explicit-not-magic `clusterName`, alternatives rejected (central bridge, distributed coordination, cluster-id in CRD).

### CRD bump (v1alpha1, additive)
- Add required `spec.serviceAccountRef.namespace` (MinLength 1, DNS-1123 pattern).
- Add required `spec.serviceAccountRef.name` (MinLength 1, DNS-1123 pattern).
- XValidation rule on `spec`: `trustPolicy.subjectMatch == "system:serviceaccount:" + serviceAccountRef.namespace + ":" + serviceAccountRef.name`.
- Regenerate manifests + sample.
- Update ADR-0004 with an addendum noting the new field (or supersede with a small ADR-0010 if cleaner).

### Bridge runtime config (read once at startup, no hot-reload)
| Env var                    | Required | Notes                                                                          |
| -------------------------- | -------- | ------------------------------------------------------------------------------ |
| `BRIDGE_CLUSTER_NAME`      | yes      | DNS-label regex `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63. Fail-fast on invalid. |
| `BRIDGE_OIDC_ISSUER`       | yes      | Used to construct go-oidc validator and to compare against `trustPolicy.issuer`. |
| `BRIDGE_HARBOR_URL`        | yes      | Base URL of the shared Harbor instance.                                        |
| `BRIDGE_HARBOR_ADMIN_FILE` | yes      | Path to mounted secret with `username` + `password` keys.                      |
| `BRIDGE_FORCE_LOCAL_VALIDATION` | no  | Default `true`. Plumbed only; `false` no-ops until upstream Harbor lands #17520. |
| `BRIDGE_LOG_LEVEL`         | no       | Default `info`.                                                                |

Log every value except admin credentials at startup.

### Harbor client wiring
- Use `github.com/goharbor/go-client` (no hand-rolled HTTP).
- Wrap with `bridge/controlplane/harbor` package exposing: `EnsureRobot`, `RotateRobotPassword`, `DeleteRobot`, `ListOwnedRobots(prefix string)`.
- Robot naming: `bridge-<cluster>-<saNs>-<saName>`. Harbor's robot username cap is documented as 255 chars (verify against go-client constants before coding); if `len(name) > capacity`, deterministic truncate to `bridge-<cluster>-` + base32(SHA-256(full-name))[:N] so reconciles remain idempotent.
- All list/delete operations apply ownership filter `strings.HasPrefix(name, "bridge-"+cluster+"-")`. Filtering is client-side; document why (Harbor robot list API has no prefix filter — verify in go-client).

### Reconciler (`bridge/controlplane/reconciler.go`)
- controller-runtime `Reconciler` watching `HarborAccess`.
- Reconcile flow:
  1. If `DeletionTimestamp` set: ensure our robot is gone (ownership-filtered), then drop finalizer.
  2. Else: add finalizer if missing.
  3. Compute desired robot name. Assert prefix is our cluster's (must be, by construction).
  4. Validate `spec.trustPolicy.issuer == BRIDGE_OIDC_ISSUER`. If not, set `Ready=False` with reason `IssuerMismatch` and stop (no Harbor call).
  5. `EnsureRobot(name, permissions)` — idempotent; rotate password if `spec.generation` advanced since `status.observedGeneration` or if `now - status.robot.lastRotated > 24h`.
  6. Update status: `robot.{name,id,passwordSecretRef,lastRotated}`, `trustPolicyEnforcedBy=bridge`, `observedGeneration`, conditions `Ready` + `RobotProvisioned` + `TrustPolicyApplied` (latter signals "enforced by bridge today").
- Password storage: per-CR `Secret` named `<ha.name>-robot-creds` in the bridge namespace, RBAC restricts read to the bridge SA. Never in the CR's namespace.

### Janitor (`bridge/controlplane/janitor.go`)
- `manager.Runnable`, ticks every 5 min (configurable).
- `ListOwnedRobots(prefix)` → for each, look up CR by reverse-mapping name; if no CR exists, delete the robot. Log every deletion with full robot name.
- Ownership filter applied at the list call; never delete or even examine a foreign-prefixed robot.

### Tests (table-driven, mocked Harbor via `httptest`)
- `robotName` correctness incl. truncation determinism.
- `ownsRobot` positive/negative incl. near-miss prefixes (`bridge-prod-eu-west-foo` must not match cluster `prod-eu-`).
- Reconcile happy path (create → status set).
- Reconcile delete-with-finalizer; verify our robot deleted, foreign robots untouched.
- Reconcile issuer-mismatch path.
- Reconcile refuses to adopt a robot with our name but missing the `harbor.aetherize.io/managed-by=bridge` label (ADR-0003 adoption discipline).
- Janitor preserves foreign robots even when no CR exists for them.
- Janitor deletes only orphan owned-prefix robots.
- XValidation rule rejects `subjectMatch` not matching `serviceAccountRef` (CRD-level test via envtest or by feeding a raw apply to a kind apiserver).

### Verification
- `make verify-package-isolation`: confirms `controlplane` does not import `dataplane` (the latter doesn't exist yet, but the check should still run).
- All unit tests pass; `go vet`, `gofmt`, build clean.

### Not in this phase
- Data Plane HTTP server. Plugin binary. Helm chart. e2e.

## Phase 3 — Data Plane

Goal: HTTPS server that validates an SA token and returns a Harbor docker bearer JWT.

- `bridge/dataplane/{server,oidc,harbor_token,cache,metrics,audit,handler}.go`.
- TLS material from cert-manager-managed paths (Phase 5 wires the chart).
- `github.com/coreos/go-oidc/v3` for SA-token validation; audience + issuer + subject all bound (`subject` derived from CR's `serviceAccountRef`).
- `/service/token` client with one-shot 401-retry path that re-reads the robot password Secret (ADR-0007).
- `ttlcache` keyed by `(ns, name, generation, subject)`; TTL = `spec.tokenTTL`.
- Prometheus metrics: `token_issuances_total`, `validation_failures_total{reason}`, `cache_hits_total`, `cache_misses_total`.
- Structured audit log line per issuance: request SA, matched HarborAccess, robot name, scope, TTL.
- `bridge/cmd/main.go` starts controller manager + data plane HTTP server in the same process (ADR-0002).
- `forceLocalValidation=false` returns 501 NotImplemented with a clear message (no alternative path yet; TODO references ADR-0009).

Tests cover: OIDC validator (valid / wrong-iss / wrong-aud / wrong-sub / expired); Harbor token client (200, 401-retry, 5xx); cache (hit, generation-miss, subject-miss, TTL expiry); handler end-to-end with mocks; `forceLocalValidation=false` path.

### Not in this phase
- Plugin binary. Helm chart. e2e.

## Phase 4 — Plugin binary

Goal: KEP-4412 plugin that the kubelet executes on the host.

- `plugin/main.go`: reads `CredentialProviderRequest` from stdin, posts SA token + image to bridge, writes `CredentialProviderResponse` to stdout.
- Env: `HARBOR_BRIDGE_ENDPOINT`, `HARBOR_BRIDGE_CA_BUNDLE`, optional `HARBOR_BRIDGE_CLIENT_CERT` / `_KEY` for mTLS.
- 5s connect timeout, 15s total.
- Returns `cacheKeyType: ServiceAccount` (ADR-0006).
- Static, CGO_ENABLED=0, multi-arch (amd64/arm64).
- Tests: round-trip with mocked bridge; timeout, 5xx, 4xx, malformed JSON paths.

### Not in this phase
- Helm chart. e2e.

## Phase 5 — Helm chart

Goal: One chart deploys the whole system; install fails clearly when misconfigured.

- Required values: `clusterName` (DNS-label validated via `required` helper), `harbor.url`, `harbor.adminCredsSecret`.
- Defaulted values: `forceLocalValidation: true`, `nodePort: 31443`, `bridge.replicas: 2`, `tls.mTLS.enabled: false`, image refs, log level.
- Templates: bridge Deployment, NodePort Service, cert-manager Certificate, ClusterRole/Binding (incl. `system:service-account-issuer-discovery`), plugin DaemonSet (binary-copier), kubelet credential-provider config file installer, CRDs.
- `NOTES.txt` prints the configured `clusterName` and warns about uniqueness across clusters.
- `helm chart-testing` (`ct lint`) clean. Golden-file render test. Install-without-clusterName must fail.

### Not in this phase
- e2e against real workloads.

## Phase 6 — e2e + finalize docs

Goal: Two kind clusters share one Harbor; each manages only its own robots.

- `test/e2e` with `sigs.k8s.io/e2e-framework`.
- Two kind clusters, different `clusterName`s. One Harbor (kind-deployed or docker-compose).
- Apply `HarborAccess` in each; deploy a test pod that pulls; assert pull succeeds.
- Assert each cluster's robot has the expected prefix; assert neither cluster's bridge ever lists, modifies, or deletes the other's robots (Harbor audit log check).
- ARCHITECTURE.md, SECURITY.md, README, MIGRATION.md extended with multi-cluster decommission walkthrough and `forceLocalValidation` flip path.
- Tag `v0.1.0`.

## Open questions / decisions pending

| Topic | Owner | Status |
| ----- | ----- | ------ |
| `serviceAccountRef` field + XValidation consistency rule | resolved 2026-05-11 | go |
| Harbor robot name length cap — verify go-client constant before coding Phase 2 truncation | Phase 2 (early) | open |
| Whether `forceLocalValidation` should default `true` after migration — revisit when Harbor #17520 lands | Phase 6 / post-migration | open |

## Cross-cutting non-goals (intentionally not in any phase)

- Central Bridge or coordinator across clusters.
- Distributed locking or shared state between bridges.
- Per-cluster identity carried inside the CRD.
- Automatic `clusterName` detection from kubeconfig context or labels.
- One-cluster-to-N-Harbors topology.
- Cross-cluster robot deduplication.
