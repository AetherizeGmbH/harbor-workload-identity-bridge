# Status and roadmap

## Shipped

| Phase | What | State |
| --- | --- | --- |
| 1 | Scaffolding, CRD types, ADRs 0001–0008 | ✅ Complete |
| 2 | Control plane: config, Harbor client, reconciler, janitor, ADRs 0009–0012 | ✅ Complete |
| 3 | Data plane: OIDC validator, HTTP handler, HTTPS server, metrics, cmd/main.go, ADR-0013 pivot | ✅ Complete |
| 4 | Plugin binary (KEP-4412 stdin/stdout protocol), ADR-0015 | ✅ Complete |
| 5 | Helm chart (bridge + plugin DaemonSet + cert-manager + kubelet config) | ✅ Complete |
| 6 | Kubelet-driven e2e + SECURITY.md polish + v0.1.0 tag | ✅ Complete; `v0.1.0` tagged, later releases cut by semantic-release ([CHANGELOG.md](../CHANGELOG.md)) |
| 7 | Harbor compatibility matrix — parameterised e2e + `harbor-compat` CI + auto-PR'd table, ADR-0020 | ✅ Complete; the weekly runs fill the table in the [README](../README.md#harbor-compatibility) |
| 8 | Cloud-agnostic node install: Go installer with `auto`/`merge`/`patch`/`none` modes, content-hash kubelet restarts, optional plugin (Talos), GKE e2e harness, ADRs 0021, 0022 and 0024 | ✅ Code + kind e2e complete; first `make e2e-gke` run against a real project still outstanding |
| 9 | Lifecycle hardening: level-triggered robot convergence, rotation-safe caching, complete revocation, HA data plane, ADRs 0023 and 0025 | ✅ Code + kind e2e (lifecycle stages) complete |
| 10 | Security review: one audience and label-selected HarborAccess objects per bridge, optional plugin namespace, audit log and rate limit, https to Harbor, token lifetime cap and pod binding, several chart-managed installs per cluster (configurable plugin provider name), refused HarborAccess objects suspend their robots, finalizer ownership, robot names Harbor accepts, Harbor client and data plane hardening, graceful shutdown and real readiness, signed releases with provenance, gosec and fuzzing, ADRs 0026–0032, [threat model](threat-models/harbor-workload-identity-bridge.md) | ✅ Complete; threat-model re-approval pending (see [Open decisions](#open-decisions-todo)) |

## Next

In order.

1. **Run the GKE e2e against a real project.** The harness exists
   (`make e2e-gke`, ADR-0022) but has not been executed yet — the first
   run validates the merge-mode runtime findings (GKE provider config
   path/format, containerd `config_path`, loopback NodePort under
   Dataplane V2) and folds them back into ADR-0022. EKS/AKS harnesses
   can follow the same module split.

2. **Other registries via a backend plugin.** Move the Harbor-specific
   code behind a translation layer so the CRD, reconciler, and data
   plane no longer depend on Harbor, then add a Nexus backend. The flow
   takes an SA token in and returns scoped credentials, which holds for
   any registry; only account provisioning and the credential handshake
   differ. It reuses the control-plane and data-plane split
   ([ADR-0002](adr/0002-bridge-control-plane-data-plane-split.md)),
   with the backend as a third seam.

## Open decisions (TODO)

The single list of decisions the maintainer still has to take, from the
2026-09 security review and the ADRs since (the threat model's open items
O1, O2, O4 and O6 among them). Each item names the options and what it
limits today.

### Threat model

- [ ] **Re-approve the threat model.** It is `Status: proposed` again:
  the closure of O3 (ADR-0028, #121: T1, T2, A5), the closure of O5 with
  the T9 change and the not yet rated T17 (ADR-0029, #130), the
  mitigations of #134 and #135, and the corrections of 2026-09-28 (T1,
  T4, T7, T16, logged events, A4) await review. *Options:* rate T17 and
  approve, or send items back. *Limits now:* the approved baseline of
  2026-09-24 no longer describes the code; nothing functional.
- [ ] **Plugin namespace split by default (O1, ADR-0027).** By default the
  privileged plugin DaemonSet runs in the release namespace, so write
  access to that namespace equals root on every node (T11);
  `plugin.namespace` (needs trust-manager) moves it out. *Options:* make
  the split the default in a future major release, or keep it opt-in.
  *Limits now:* in the default layout the release namespace must allow
  privileged pods and be restricted like cluster-admin.
- [ ] **mTLS client identity (O2).** With mTLS on, the bridge accepts any
  client certificate its CA signed; with a shared ClusterIssuer, anyone who
  may create cert-manager Certificates can get one. *Options:* a dedicated
  client CA plus an identity check, and whether mTLS should be on by
  default. *Limits now:* nothing functional; mTLS is optional defense in
  depth, and every request still needs a valid, audience-bound
  ServiceAccount token.
- [ ] **Repository settings (O4).** Required review and CODEOWNERS, secret
  scanning and push protection, private vulnerability reporting, the
  release App's ruleset bypass, and the "Dependabot alerts: read"
  permission the Renovate App lacks are GitHub settings, not code.
  *Options:* enable each, or record why not. *Limits now:* the supply-chain
  residual (T15) stays Medium.
- [ ] **CA and mTLS files out of the sync containers' reach (O6,
  ADR-0029, #130).** Every release's sync container mounts
  `plugin.hostConfigDir` read-write as root; it holds kubelet's
  chart-owned config (patch and none mode) and every release's CA pin and
  mTLS key. A compromised sync container can swap its own config in right
  before a kubelet start, and so redirect another release's pods' tokens,
  and can read other releases' mTLS keys and replace their CA pins
  (SECURITY.md, T17). *Options:* a directory per release for the CA and
  mTLS files that only that release's sync container mounts, with the
  config in a directory only installers mount; it moves the CA file every
  entry names, so the upgrade restarts kubelet once per node and install.
  Or accept the risk. *Limits now:* with one release per node, only that
  release's own sync container can do this, to its own pulls; with several
  releases sharing `plugin.hostConfigDir` (always in patch and none mode),
  any release's sync container can redirect another release's tokens and
  read its mTLS key.

### Data plane

- [ ] **Per-source rate limit behind SNAT.** The credential endpoint's
  token bucket is keyed by the TCP peer (`bridge/dataplane/ratelimit.go`).
  With the Service's default `externalTrafficPolicy: Cluster` a node's
  NodePort usually SNATs callers to that node's address, so anyone who can
  reach a node's NodePort can use up the bucket its kubelet shares
  (`bridge.rateLimit.perSource: 20`, `burst: 100`), and that node's pulls
  of matched images get `429`. *Options:* a second, per-subject limit after
  the token check; `externalTrafficPolicy: Local`, which keeps client
  addresses but serves external callers only on nodes with a bridge replica
  (what it does to kubelet's loopback calls on the other nodes depends on
  the service proxy and needs testing); or accept it and firewall the
  NodePort (SECURITY.md). *Limits now:* a caller on the node network can
  hold up new pulls on one node; kubelet's cached credentials still cover
  recent ones.
- [ ] **Uncounted wrong-method and wrong-path requests.** The credential
  listener answers `405` and `404` before it counts or logs anything
  (`bridge/dataplane/handler.go`). *Options:* a new
  `bridge_credential_issuances_total` result value, which changes the
  metrics contract (sums over all results), or a counter of its own.
  *Limits now:* scans of the NodePort with other paths or methods leave no
  trace in the bridge.

### Control plane and chart

- [ ] **Two releases in one namespace.** The leader-election Lease has a
  fixed name, `bridge.harbor.aetherize.io` (`bridge/cmd/main.go`), and the
  janitor deletes the robot Secrets of its `clusterName` in its namespace
  whose HarborAccess its selector does not match, so the docs require a
  namespace per release ([Several installs per
  cluster](operations.md#several-installs-per-cluster-adr-0029)). *Options:* derive the
  Lease name from `BRIDGE_INSTANCE` and scope the Secret sweep to the
  instance; an upgrade that renames the Lease lets an old and a new replica
  lead at the same time during the rollout unless it is handled. Or keep
  the rule. *Limits now:* each release needs a namespace of its own.
- [ ] **Cluster-scoped names without the namespace.** The audience
  ClusterRole and ClusterRoleBinding (`<fullname>-audience-token-request`)
  and, with `plugin.namespace`, the trust-manager Bundle are named after the
  release only, while the bridge's own ClusterRole carries the namespace,
  so two releases with the same name in different namespaces collide.
  *Options:* add the namespace to those names; that renames cluster-scoped
  RBAC on upgrade, and kubelet's token requests for the audience fail
  while the old binding is gone and the new one not yet in effect (Helm
  creates new objects before it deletes old ones; other deploy tools may
  not). Or keep the rule. *Limits now:* release names must be unique per
  cluster.
- [ ] **Finalizers left by a removed selector or a renamed instance
  (ADR-0032).** The chart renders `BRIDGE_INSTANCE` only together with
  `bridge.harborAccessSelector`, so after the selector is removed through
  the chart the bridge cannot name `harbor.aetherize.io/robot-<instance>`,
  and a renamed `bridge.instance` never releases the old one (see
  [Uninstalling](operations.md#uninstalling)). *Options:* always render
  `BRIDGE_INSTANCE`, which the bridge already accepts without a selector
  and then releases the per-instance finalizer; or a
  `BRIDGE_PREVIOUS_INSTANCES` setting naming instances to release.
  *Limits now:* changing either setting needs a manual finalizer cleanup,
  and until then the objects cannot finish deletion.
- [ ] **Overlapping selectors (ADR-0026, ADR-0032).** When two bridges'
  selectors both match a HarborAccess, the bridge whose audience it names
  serves it and the other reports `AudienceMismatch`; both write the same
  `Ready` condition, so its status flips whenever either reconciles it.
  Whether the shared finalizer is a bridge's own (`servedHere`,
  `statusRobotIsOurs`) is decided by the `clusterName` in `status.robot`,
  which only bridges sharing a Harbor must keep distinct. *Options:*
  conditions per bridge instance, or forbid overlap (an admission policy,
  or a bridge that leaves objects another instance serves alone).
  *Limits now:* selectors must not overlap, and bridges on one cluster
  should use distinct `clusterName` values even with different Harbors.
- [ ] **No values schema.** The chart has no `values.schema.json`, so a
  misspelt key is ignored silently and its default applies; for example
  `bridge.mTLS.enable: true` leaves mTLS off. *Options:* a strict schema,
  which rejects unknown keys at template time but breaks `helm upgrade`
  for existing values files with stale or unknown keys; or a schema that
  types the known keys and allows others. *Limits now:* a typo in a
  security switch goes unnoticed.
- [ ] **Issuer-discovery RBAC.** The chart binds no role for OIDC
  discovery and the JWKS; the bridge relies on Kubernetes' default
  `system:service-account-issuer-discovery` binding for all
  ServiceAccounts (ADR-0006 status note). *Options:* have the chart bind
  the role to the bridge's ServiceAccount, or document the reliance only.
  *Limits now:* on a cluster that removed the default binding the bridge
  cannot fetch the signing keys and exits at startup.

### Nodes

- [ ] **Node-side uninstall.** `helm uninstall` leaves kubelet wired to the
  plugin: the provider entry, binary, record, CA, mTLS and state files stay
  until removed by hand ([Uninstalling](operations.md#uninstalling)). *Options:* a
  pre-delete hook that removes this release's entry and files on every
  node and restarts kubelet, which needs the installer's privileges at
  uninstall time, has to reach every node, and must leave other releases'
  entries alone; or keep the manual procedure. *Limits now:* every
  uninstall or `plugin.providerName` rename needs the manual steps on every
  node; until then kubelet runs a plugin that fails for its `matchImages`.

### Release and development

- [ ] **Vulnerability gate at release time.** `release-images.yml` pushes
  and signs each image right after the build; its Trivy job only generates
  the SBOM. `govulncheck` and the Trivy filesystem scan
  (`vulnerabilities.yml`) run on pull requests, pushes to `main` and once a
  day, not on the release artefacts. *Options:* `govulncheck -mode=binary`
  on the built binaries, or a Trivy image scan with a severity gate, before
  the images are pushed and signed; that can block a release on a
  vulnerability disclosed after the merge. *Limits now:* a vulnerability
  disclosed less than a day before the release is published and signed
  without a check; one disclosed earlier is red in the daily scan of
  `main`, which does not stop the release.
