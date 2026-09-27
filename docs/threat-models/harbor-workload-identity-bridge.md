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
format of the DSOMM-based ai-security-rules and was approved by the
maintainer on 2026-09-24. Changes to the threat model need a new
approval; the closure of O3 (ADR-0028, 2026-09-27: T1, T2, A5), and T17
(not yet rated) and the T9 change with the closure of O5 (ADR-0029,
2026-09-27), await it.

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
| T1 | S | B1 | Forged, foreign-audience, long-lived or unbound token redeemed for credentials | OIDC signature, issuer, expiry (go-oidc, `bridge/dataplane/oidc.go`), with signing keys fetched over https only (plain http only to a loopback host, `oidc.go`); lifetime `exp - iat` at most `bridge.tokenValidation.maxLifetime` (1h, what kubelet's tokens have), `iat` required, and the `kubernetes.io` pod claim required, so a `kubectl create token --duration=8760h` token or one bound to no pod is refused ([ADR-0028](../adr/0028-token-lifetime-cap-and-pod-binding.md), `oidc.go`); exactly one served audience, CR must name it ([ADR-0026](../adr/0026-audience-pinning-and-harboraccess-selector.md)); subject must match `serviceAccountRef` | Low (2.4), see A5 |
| T2 | S | B1 | Pod-creator mounts a bridge-audience token and fetches its own SA's password (abuse story 2) | Same credential the workload already gets through kubelet; limited to that SA's grants; the token must be pod-bound and live at most 1h, so a projected token with a longer `expirationSeconds` is refused ([ADR-0028](../adr/0028-token-lifetime-cap-and-pod-binding.md)); mTLS (optional) and a NetworkPolicy restrict who can reach the endpoint | Medium (4.8), see A2 and A5 |
| T3 | T | B1 | Man in the middle between plugin and bridge, or a redirect that carries the pod token away | TLS 1.2+, plugin pins the bridge CA, `127.0.0.1` NodePort by default, verifies against the Service name for `$(NODE_IP)` endpoints; no redirects followed (`plugin/bridge_client.go`); optional mTLS | Low (2.0) |
| T4 | D | B1 | Flood of requests or forged tokens | Per-source token bucket, 429 before parsing (`ratelimit.go`); JWKS refetch at most every 30s (`oidc_keyset.go`); 64 KiB body, 32 KiB headers, server timeouts; PDB with two replicas | Medium (4.2) |
| T5 | I | B2 | Bridge SA token leaks to a non-apiserver host or redirect target | Token only over https to the in-cluster apiserver and its discovery-named `jwks_uri`; redirects not followed (`oidc_client.go`) | Low (1.8) |
| T6 | E | B5 | CR author grants `*` (every Harbor project) | Harbor project-name pattern in the CRD, the reconciler and the client (`harboraccess_types.go`, `naming.go`) | Low (2.0) |
| T7 | E | B5 | CR author reaches another cluster's or CR's robot | Dot-delimited injective naming ([ADR-0018](../adr/0018-dot-delimited-naming.md)); three-layer ownership (prefix, cluster tag, owning CR) before any update or delete (`contract.go`); fuzzed (`FuzzRobotName_Injective`) | Low (1.6) |
| T8 | E | B5 | Anyone allowed to create HarborAccess objects grants any project to any SA | Documented as cluster-privileged; restrict RBAC and gate with admission policy (SECURITY.md "Unauthorized HarborAccess authorship") | Medium (5.0), see A1 |
| T9 | E | B6 | Symlink planted next to installer files redirects a root write or read | `os.Root`, Lstat + `os.SameFile`, `O_NOFOLLOW`, O_EXCL temp + rename; `plugin.hostConfigDir` opened one component at a time, refusing a symlink in its path (`installer/files.go`); nested per-release directories: T17 | Low (2.2) |
| T10 | S/E | B6 | Fake kubelet process steers where the installer writes | PPID 1, readable non-pod cgroup, shared host mount namespace, refuse ambiguity (`installer/discover.go`) | Low (2.4) |
| T11 | E | B6 | Write access to the namespace of the privileged plugin DaemonSet equals root on every node | Optional split: `plugin.namespace` runs the DaemonSet in its own `privileged` namespace, the CA arrives through a trust-manager Bundle, the bridge namespace can enforce `restricted` ([ADR-0027](../adr/0027-optional-plugin-namespace.md)); SECURITY.md states the default layout's equivalence | High (6.8) in the default layout; Low (3.0) with the split |
| T12 | I | B4 | Harbor admin credential or robot passwords in logs or on the wire | SDK wire dumps disabled regardless of `DEBUG` (`harbor/client.go`); https to Harbor required, plain http only by explicit opt-in, private CA via `harbor.caSecret`; the Harbor client follows no redirects, which would re-send the admin credential, possibly over http (`harbor/client.go`); audit lines never carry secrets (tested) | Low (1.6) |
| T13 | D | B4 | Hung Harbor blocks all reconciles and revocations | 30s per call, header/TLS timeouts, page cap (`harbor/client.go`); `DeletionBlocked` on error | Low (2.6) |
| T14 | R | B1/B5 | Credential issuance or denial cannot be attributed | Audit logger fixed at info; source IP, client cert, pod, node, reason per decision (`handler.go`); Kubernetes audit log covers CR changes | Low (2.4) |
| T15 | T | B7 | Malicious dependency or tool version reaches a signed release, or a re-pointed tag reaches the nodes | Actions SHA-pinned; Trivy image pinned by digest; Renovate waits 7 days and never automerges majors; images built from the tag without cache; Trivy isolated without rights; cosign signatures, SBOM and SLSA provenance on every image and the chart; images pinnable by digest (`*.image.digest`) | Medium (4.4), see O4 |
| T16 | I | B3 | Robot Secrets readable by others in the bridge namespace, or a foreign Secret served as credentials | Secrets live only in the bridge namespace; the data plane serves only Secrets the bridge labelled, the reconciler never adopts a foreign one; RBAC trimmed to the verbs used; restrict namespace RBAC; enable encryption at rest (operator) | Medium (4.0), see A3 |
| T17 | T/D/I/E | B6 | Several installs on one node: one installer drops or replaces another install's or the cloud's provider entry, overwrites a foreign binary (GKE keeps kubelet in the provider bin dir) through its provider name, or two installers race on the shared config and the kubelet restart; or a writer of `plugin.hostConfigDir` (every release's sync container) plants or changes an entry in a shared chart-owned config that an installer keeps, or that kubelet reads at a start, e.g. to send another release's pods' tokens elsewhere, or plants one whose binary is missing, so that kubelet exits at the restart; or that writer reads or replaces another release's CA pin and mTLS key in the shared directory, or redirects the installer's writes through a symlink in place of a release's `plugin.hostConfigDir` nested in its own | Per-install provider name and files; in the cloud's config each installer replaces only a bridge entry of its own name and refuses to write, or restart kubelet onto, a config kubelet exits on (missing or non-executable binary, repeated or invalid name); every installer records its entry's canonical bytes, and the chart-owned config it writes into, in `<bin dir>/<name>.entry`, adding a new entry before it writes the entry and dropping the old one only after; a chart-owned config (patch/none, and in merge mode kubelet's config when it is this install's own or a record names it) keeps only this install's rendered entry and other installs' entries whose executable binary is in kubelet's bin dir and whose record there holds that entry; merge mode refuses any other kubelet config, and `plugin.install.configFile`, inside `plugin.hostConfigDir`; the chart and the installer refuse a bin dir or state dir in reach of `plugin.hostConfigDir`, and no sync container mounts the bin dir; a file in the bin dir counts as this install's only with its record or its exact bytes; a refused pass changes no config, binary, record, CA, mTLS or state file (it may leave lock files); patch mode does not rewire kubelet away from another install's entry (in a config in reach of a `plugin.hostConfigDir`, only a recorded one counts) and holds the locks of kubelet's current config and its own from before it checks them to the end of the pass; flock node and config locks around every read-modify-write and restart; the installer opens `plugin.hostConfigDir` component by component and refuses a symlink in its path; the docs require disjoint per-release directories and rule out `plugin.hostConfigDir` as kubelet's config directory ([ADR-0029](../adr/0029-configurable-plugin-provider-name.md), `installer/lock.go`, `installer/record.go`, `installer/run.go`, `installer/providerconfig.go`, `installer/files.go`). Residual: kubelet reads a chart-owned config whenever it starts, and nothing reads it again after the installer's write, so a writer of `plugin.hostConfigDir` that swaps its own version in between an installer's write and the kubelet restart that installer triggers (inotify shows it when), or before any other kubelet start (a node reboot), has kubelet run a changed or planted entry until the next kubelet start that reads a clean file, and the installer records success: with another release's `matchImages` and audience and its own endpoint, it receives that release's pods' tokens, which that release's bridge accepts; before ADR-0029 only a release's own sync container had this reach, over its own pulls; taking the config out of the sync containers' reach (a per-release directory for the CA and mTLS files that only that release's sync container mounts) would close it, the reboot case and the key disclosure below, and is an open decision. A writer of `plugin.hostConfigDir` can also remove another install's entry (or change it so that the next pass drops it) until that install's next pass, a denial of service against its pulls; after another install's pass died between its record and its config, it can switch that install's entry between the old and the new one until that install's next pass; merge mode keeps the entries of a kubelet config no record names as chart-owned (an installer before ADR-0029, which "Upgrade first" rules out, or a config placed in a `plugin.hostConfigDir` by hand); a `plugin.hostConfigDir` that is kubelet's config directory (Kubernetes 1.34+), which the docs rule out, lets that writer add config files kubelet loads at every start and no pass removes (patch mode moves kubelet off it; none mode cannot see kubelet's flags); with a shared `plugin.hostConfigDir` (required in patch and none mode) a compromised sync container can read the other releases' mTLS client keys and replace their pinned CA, which each release restores within one sync interval (60 s): a denial of service, or impersonation of that bridge together with a position on the path to its endpoint; a per-release `plugin.hostConfigDir` inside another release's is in that release's reach in the same way, and kubelet follows a symlink planted in its place when it mounts it into a `none`-mode install container or a sync container, which the installer cannot see; no component compares directories across releases | Not rated: the maintainer rates T17 at approval (drafted as Low (2.2) before the swap before a kubelet start, and with it cross-release token redirection, was known; it also carries the cross-release disclosure of mTLS client keys and replacement of CA pins in the shared directory) |

Security events the system must log (acceptance criteria):

- every credential issuance and denial, with reason, source and pod/node
  attribution (`audit` logger, `credential issued` / `credential denied`,
  and `credential unavailable` for a valid token answered with 503 or 500);
- token validation failures by category, including `excessive_lifetime`
  and `not_pod_bound` (metric `bridge_oidc_validation_failures_total`,
  and the `category` of the `credential denied` audit line);
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
- **A5 — A stolen pod-bound token outlives its pod at the bridge.** The
  bridge validates tokens locally, so a stolen kubelet token stays
  redeemable for its remaining lifetime, at most
  `bridge.tokenValidation.maxLifetime` (1h), after its pod or its
  ServiceAccount is deleted. The robot password it redeems works at
  Harbor until the next rotation, which the bridge schedules 24h and 1m
  after the previous one (`bridge/controlplane/contract.go`), unless the
  password Secret is deleted first. Whoever may create tokens for a
  ServiceAccount can mint a pod-bound one for an existing pod of it; one
  token per rotation, about one TokenRequest a day, keeps them supplied.
  Justification: only the apiserver's TokenReview checks the bound
  object, at the cost of an apiserver round trip per pull
  ([ADR-0028](../adr/0028-token-lifetime-cap-and-pod-binding.md)).

Open items (not accepted; tracked):

- **O1** Namespace split for the plugin DaemonSet (T11) — implemented as an
  option (ADR-0027, requires trust-manager); the default layout keeps the
  risk. Decide whether a future major release makes the split the default.
- **O2** mTLS client identity: dedicated client CA and identity pinning;
  mTLS is off by default.
- ~~**O3** Token lifetime cap and pod binding for credential requests.~~
  Closed by ADR-0028 (2026-09-27): mitigations in T1 and T2, residual
  risk A5.
- **O4** Repository settings: required review and CODEOWNERS, secret
  scanning and push protection, private vulnerability reporting, and the
  release App's ruleset bypass; the Renovate App lacks "Dependabot alerts:
  read".
- ~~**O5** Configurable plugin provider names, so several chart-managed
  plugins can coexist on a node (ADR-0026).~~ Closed by ADR-0029
  (2026-09-27): implemented as `plugin.providerName`, mitigations in T17.
  `helm uninstall` still leaves an install's provider entry and files on
  the nodes, as it always did; removing them is manual (README,
  "Uninstalling").
- **O6** Chart-owned config in reach of the sync containers (T17): every
  release's sync container can write `plugin.hostConfigDir`, which holds
  kubelet's chart-owned config and, with several releases, every
  release's CA pin and mTLS key. Decide on a directory per release for
  the CA and mTLS files that only that release's sync container mounts,
  with the config in a directory only installers mount. It moves the CA
  file every entry names (one kubelet restart per install on upgrade)
  and closes the swap before a kubelet start, the reboot case and the
  cross-release key disclosure.
