# Threat model: Harbor Workload Identity Bridge

Status: proposed
Approved-by:
Approved-at:

Applies-to:
- `bridge/**`
- `plugin/**`
- `installer/**`
- `charts/harbor-bridge/**`
- `.github/workflows/**`

This is the system-level model after the 2026-09 security review. It
complements [SECURITY.md](../../SECURITY.md) (the operator-facing threat
model and hardening guide) with STRIDE per trust boundary, DREAD-rated
residual risks and the security events the system logs. It follows the
format of the DSOMM-based ai-security-rules; it is `proposed` until a
maintainer reviews and approves it.

## Scope and protection requirement

- **Data:** Harbor robot credentials (username/password with push or pull
  rights on Harbor projects), the Harbor admin or system-robot credential,
  Kubernetes ServiceAccount tokens (bound, audience-scoped), TLS and mTLS
  private keys. Credentials only; no PII beyond Kubernetes object names.
- **Exposure:** the credential endpoint listens on a NodePort on every
  node (reachable from every node network and, through the Service's
  ClusterIP, from every pod). The control plane talks to the Kubernetes
  API and to Harbor. The installer runs privileged on every node.
- **Regulation:** none specific. A leaked robot credential is a supply
  chain risk (image push), so integrity controls are treated as high.
- **Classification:** high. Credentials with write access to a registry,
  a privileged node component, and an internet-reachable endpoint where
  node pools have public addresses.

Data flow and trust boundaries:

```
workload pod ──(kubelet mints bound SA token, aud=plugin.audience)──▶ kubelet
kubelet ──exec──▶ plugin (root, node) ──HTTPS (+mTLS) via NodePort──▶ bridge data plane   [B1: node → cluster network]
bridge ──OIDC discovery / JWKS (bridge SA token, apiserver only)──▶ kube-apiserver         [B2]
bridge ──list/watch HarborAccess, read/write robot Secrets──▶ kube-apiserver               [B3]
bridge ──Harbor REST (admin/system robot, Basic auth, TLS)──▶ Harbor                       [B4: cluster → Harbor]
HarborAccess author ──kubectl / GitOps──▶ kube-apiserver ──▶ bridge                        [B5: tenant → control plane]
installer (privileged, hostPID, host root) ──▶ node files, kubelet restart                 [B6: pod → node]
CI / release ──build, sign, attest──▶ ghcr.io ──▶ nodes and bridge                        [B7: supply chain]
```

## User story and abuse stories

User story: as a platform operator I want workloads to pull from Harbor
with the identity they already run as, so that no long-lived pull secret
lives in any workload namespace.

Abuse stories:

1. As an attacker on the node network, I send forged or replayed tokens to
   the NodePort to get robot credentials, or to exhaust the bridge.
2. As a tenant who may create pods, I mount a projected token for the
   bridge audience and ask for my ServiceAccount's robot password
   directly, bypassing kubelet.
3. As a HarborAccess author, I grant myself projects I should not have,
   name another audience, or target another tenant's or cluster's robot.
4. As a pod on a node, I plant symlinks or fake a kubelet process to make
   the privileged installer write or read files as root.
5. As a man in the middle (or a compromised host behind a redirect), I
   capture the bridge's own token or the Harbor admin credential.
6. As an attacker in the supply chain, I publish a malicious dependency
   or tool version and wait for it to be built and signed into a release.
7. As an insider, I use the bridge and cover my tracks.

## Threats and mitigations

| # | STRIDE | Boundary | Threat | Mitigation (where) | Residual DREAD (estimate) |
| --- | --- | --- | --- | --- | --- |
| T1 | S | B1 | Forged or foreign-audience token redeemed for credentials | OIDC signature, issuer, expiry (go-oidc, `bridge/dataplane/oidc.go`); exactly one served audience, CR must name it ([ADR-0026](../adr/0026-audience-pinning-and-harboraccess-selector.md)); subject must match `serviceAccountRef` | Low (2.4) |
| T2 | S | B1 | Pod-creator mounts a bridge-audience token and fetches its own SA's password (abuse story 2) | Same credential the workload already gets through kubelet; limited to that SA's grants; mTLS (optional) and a NetworkPolicy restrict who can reach the endpoint | Medium (4.8), see A2 |
| T3 | T | B1 | Man in the middle between plugin and bridge, or a redirect that carries the pod token away | TLS 1.2+, plugin pins the bridge CA, `127.0.0.1` NodePort by default, verifies against the Service name for `$(NODE_IP)` endpoints; no redirects followed (`plugin/bridge_client.go`); optional mTLS | Low (2.0) |
| T4 | D | B1 | Flood of requests or forged tokens | Per-source token bucket, 429 before parsing (`ratelimit.go`); JWKS refetch at most every 30s (`oidc_keyset.go`); 64 KiB body, 32 KiB headers, server timeouts; PDB with two replicas | Medium (4.2) |
| T5 | I | B2 | Bridge SA token leaks to a non-apiserver host or redirect target | Token only over https to the in-cluster apiserver and its discovery-named `jwks_uri`; redirects not followed (`oidc_client.go`) | Low (1.8) |
| T6 | E | B5 | CR author grants `*` (every Harbor project) | Harbor project-name pattern in the CRD, the reconciler and the client (`harboraccess_types.go`, `naming.go`) | Low (2.0) |
| T7 | E | B5 | CR author reaches another cluster's or CR's robot | Dot-delimited injective naming ([ADR-0018](../adr/0018-dot-delimited-naming.md)); three-layer ownership (prefix, cluster tag, owning CR) before any update or delete (`contract.go`); fuzzed (`FuzzRobotName_Injective`) | Low (1.6) |
| T8 | E | B5 | Anyone allowed to create HarborAccess objects grants any project to any SA | Documented as cluster-privileged; restrict RBAC and gate with admission policy (SECURITY.md "Unauthorized HarborAccess authorship") | Medium (5.0), see A1 |
| T9 | E | B6 | Symlink planted next to installer files redirects a root write or read | `os.Root`, Lstat + `os.SameFile`, `O_NOFOLLOW`, O_EXCL temp + rename (`installer/files.go`) | Low (2.2) |
| T10 | S/E | B6 | Fake kubelet process steers where the installer writes | PPID 1, readable non-pod cgroup, shared host mount namespace, refuse ambiguity (`installer/discover.go`) | Low (2.4) |
| T11 | E | B6 | Write access to the namespace of the privileged plugin DaemonSet equals root on every node | Optional split: `plugin.namespace` runs the DaemonSet in its own `privileged` namespace, the CA arrives through a trust-manager Bundle, the bridge namespace can enforce `restricted` ([ADR-0027](../adr/0027-optional-plugin-namespace.md)); SECURITY.md states the default layout's equivalence | High (6.8) in the default layout; Low (3.0) with the split |
| T12 | I | B4 | Harbor admin credential or robot passwords in logs or on the wire | SDK wire dumps disabled regardless of `DEBUG` (`harbor/client.go`); https to Harbor required, plain http only by explicit opt-in, private CA via `harbor.caSecret`; audit lines never carry secrets (tested) | Low (1.6) |
| T13 | D | B4 | Hung Harbor blocks all reconciles and revocations | 30s per call, header/TLS timeouts, page cap (`harbor/client.go`); `DeletionBlocked` on error | Low (2.6) |
| T14 | R | B1/B5 | Credential issuance or denial cannot be attributed | Audit logger fixed at info; source IP, client cert, pod, node, reason per decision (`handler.go`); Kubernetes audit log covers CR changes | Low (2.4) |
| T15 | T | B7 | Malicious dependency or tool version reaches a signed release, or a re-pointed tag reaches the nodes | Actions SHA-pinned; Trivy pinned; Renovate waits 7 days and never automerges majors; images built from the tag without cache; Trivy isolated without rights; cosign signatures, SBOM and SLSA provenance on every image and the chart; images pinnable by digest (`*.image.digest`) | Medium (4.4), see O4 |
| T16 | I | B3 | Robot Secrets readable by others in the bridge namespace, or a foreign Secret served as credentials | Secrets live only in the bridge namespace; the data plane serves only Secrets the bridge labelled, the reconciler never adopts a foreign one; RBAC trimmed to the verbs used; restrict namespace RBAC; enable encryption at rest (operator) | Medium (4.0), see A3 |

Security events the system must log (acceptance criteria):

- every credential issuance and denial, with reason, source and pod/node
  attribution (`audit` logger, `credential issued` / `credential denied`);
- token validation failures by category (metric
  `bridge_oidc_validation_failures_total`);
- rate-limited requests (`bridge_credential_issuances_total{result="rate_limited"}`);
- robot creation, update, rotation and deletion, with the owning CR
  (controller log);
- janitor revocations and finalizer releases, with the reason;
- `Ready=False` reasons on the HarborAccess (`AudienceMismatch`,
  `InvalidSpec`, `DeletionBlocked`, `RobotDisabled`, `IssuerMismatch`).

Deviation from the rule set: the rules require OPA/Rego for every
authorization decision. This system authorizes with Kubernetes RBAC,
CRD validation and the bridge's own checks (issuer, audience, subject,
ownership); a policy engine for HarborAccess authorship belongs to the
operator's admission control (SECURITY.md recommends one).

## Accepted risks

- **A1 — HarborAccess authorship is cluster-privileged.** Whoever may
  create a HarborAccess can grant any Harbor project to any SA identity in
  the cluster. Justification: the CR is the policy; constraining it per
  tenant is an admission-control concern (Kyverno/Gatekeeper/VAP).
  Owner: platform operator.
- **A2 — A pod that runs as SA X can obtain X's robot password.**
  Justification: the password is the credential the workload is entitled
  to; this is no worse than an `imagePullSecret`. mTLS and a NetworkPolicy
  narrow who can reach the endpoint.
- **A3 — Robot passwords are stored in Kubernetes Secrets.**
  Justification: kubelet's plugin API needs a password; Secrets with
  namespace-scoped RBAC and encryption at rest are the platform's store.
- **A4 — Kubelet caches credentials up to `tokenTTL`.** A revoked robot's
  cached credentials fail at Harbor immediately; a rotated one fails pulls
  until the cache entry expires. Justification: kubelet has no
  invalidation API.

Open items (not accepted; tracked):

- **O1** Namespace split for the plugin DaemonSet (T11) — implemented as an
  option (ADR-0027, requires trust-manager); the default layout keeps the
  risk. Decide whether a future major release makes the split the default.
- **O2** mTLS client identity: dedicated client CA and identity pinning;
  mTLS is off by default.
- **O3** Token lifetime cap and pod binding for credential requests.
- **O4** Repository settings: required review and CODEOWNERS, secret
  scanning and push protection, private vulnerability reporting, and the
  release App's ruleset bypass; the Renovate App lacks "Dependabot alerts:
  read"; Renovate cannot look up `aquasecurity/*` (IP allow list).
- **O5** Configurable plugin provider names, so several chart-managed
  plugins can coexist on a node (ADR-0026).
