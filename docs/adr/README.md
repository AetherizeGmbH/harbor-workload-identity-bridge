# Architecture Decision Records

| #    | Title                                                                                  | Status   |
|------|----------------------------------------------------------------------------------------|----------|
| 0001 | [Record architecture decisions](0001-record-architecture-decisions.md)                 | Accepted |
| 0002 | [Bridge split into Control Plane and Data Plane](0002-bridge-control-plane-data-plane-split.md) | Accepted (amended by 0025) |
| 0003 | [Persistent Harbor robots per HarborAccess CR](0003-persistent-robots-per-harboraccess.md) | Accepted (naming superseded by 0009 and 0018; docker-token and password points reversed by 0013; label adoption replaced by the description contract of 0012; rotation and deletion amended by 0023) |
| 0004 | [trustPolicy as a first-class CRD field](0004-trust-policy-as-crd-field.md)            | Accepted |
| 0005 | [Docker token issuance via Harbor /service/token](0005-docker-token-via-service-token.md) | Superseded by 0013 |
| 0006 | [OIDC validation strategy and audience binding](0006-oidc-validation-and-audience.md)  | Accepted (default issuer is `https://kubernetes.default.svc.cluster.local`; the bridge's ClusterRole holds the issuer-discovery rules; audience pinned by 0026; extended by 0028) |
| 0007 | [Cache invalidation when HarborAccess CRs change](0007-cache-invalidation-on-cr-change.md) | Superseded by 0013 |
| 0008 | [Plugin-to-bridge transport: NodePort](0008-plugin-to-bridge-transport-nodeport.md)    | Accepted (amended by 0025; the chart supports only a fixed `service.nodePort`; refined by 0042) |
| 0009 | [Multi-cluster topology](0009-multi-cluster-topology.md)                               | Accepted (naming scheme + hyphen-prefix caveat superseded by 0018; §6 "stops without touching Harbor" superseded by 0030) |
| 0010 | [ServiceAccountRef as the canonical workload identity in HarborAccess](0010-service-account-ref-as-identity.md) | Accepted |
| 0011 | [Robot password storage as per-CR Kubernetes Secret in the bridge namespace](0011-robot-password-secret-storage.md) | Accepted (Secret name superseded by 0018; the chart grants the bridge Secret access but cannot restrict others) |
| 0012 | [Robot description as the cross-component reconciler↔janitor contract](0012-robot-description-as-component-contract.md) | Accepted (helpers in `contract.go`; amended by 0023; `suspended=true` token added by 0030) |
| 0013 | [Return robot Basic Auth credentials, not pre-minted Docker JWTs](0013-return-robot-basic-auth-credentials.md) | Accepted (supersedes 0005, supersedes 0007; reverses the docker-token and password points of 0003; cache duration amended by 0023) |
| 0014 | [Handle Harbor's `robot$` prefix asymmetry at the comparison boundary](0014-harbor-robot-dollar-prefix-handling.md) | Accepted (normalization point moved into the Harbor client by 0023) |
| 0015 | [The plugin duplicates wire types instead of importing them](0015-plugin-duplicates-wire-types.md) | Accepted (enum-mismatch reasoning corrected by 0016; since #111 the plugin does validate `cacheKeyType`; the JSON keys are pinned by the golden files in `bridge/dataplane/testdata`, read by both sides' tests) |
| 0016 | [`cacheKeyType` in the credential-provider response is `Registry`, not `ServiceAccount`](0016-credential-provider-cache-key-type.md) | Accepted (supersedes the enum claim in 0015; `tokenAttributes.cacheType` is no SA-token cache: with `tokenAttributes` set kubelet keys cached credentials per ServiceAccount under either value, `Token` only adds the token's hash; and `cacheDuration`, not the token lifetime, bounds plugin runs) |
| 0017 | [The chart provisions audience-scoped RBAC for kubelet token requests](0017-chart-provisions-audience-rbac.md) | Accepted |
| 0018 | [Dot-delimited robot and Secret names for collision-free identity mapping](0018-dot-delimited-naming.md) | Accepted (supersedes 0009 naming scheme and 0011 Secret name; Harbor-validity and truncation claims amended by 0031) |
| 0019 | [Code conventions and CI quality gates](0019-code-conventions-and-ci-quality-gates.md) | Accepted (vulnerability checks on pull requests amended by 0036) |
| 0020 | [Harbor compatibility is tested, not asserted](0020-harbor-compatibility-matrix.md) | Accepted (GetByID removed and UpdatePermissions replaced by Update, 0023) |
| 0021 | [Node installer: Go binary with auto/merge/patch/none modes](0021-node-installer-modes.md) | Accepted |
| 0022 | [GKE e2e harness for merge-mode installation](0022-gke-e2e-harness.md) | Accepted (never run yet) |
| 0023 | [Level-triggered robot lifecycle, rotation-safe credential caching, and complete revocation](0023-level-triggered-robot-lifecycle.md) | Accepted (amends 0003, 0012, 0013, 0014; a disabled robot marked as suspended is the bridge's to resume, amended by 0030) |
| 0024 | [The chart can leave the plugin to the platform; supported node platforms](0024-plugin-optional-and-platform-support.md) | Accepted (extends 0021) |
| 0025 | [Data plane serves on every replica; metrics leave the credential listener](0025-data-plane-serving.md) | Accepted (amends 0002, 0008; since 0.11.2 a replica is ready only once its caches have synced) |
| 0026 | [One bridge serves one audience and a selected set of HarborAccess objects](0026-audience-pinning-and-harboraccess-selector.md) | Accepted (extends 0006, 0010, 0017, 0023; "no robot" for a refused object that had one amended by 0030; finalizer placement and release amended by 0032; one plugin per node refined by 0029) |
| 0027 | [The plugin DaemonSet can run in its own namespace](0027-optional-plugin-namespace.md) | Accepted (extends 0021, 0024) |
| 0028 | [Token lifetime cap and pod binding](0028-token-lifetime-cap-and-pod-binding.md) | Accepted (extends 0006, 0010) |
| 0029 | [Configurable plugin provider name](0029-configurable-plugin-provider-name.md) | Accepted (refines 0021, 0026; "helm uninstall removes nothing on the nodes" replaced by 0039) |
| 0030 | [A refused HarborAccess suspends its robot](0030-refused-harboraccess-suspends-its-robot.md) | Accepted (supersedes part of 0009 §6, amends 0023, 0026; Harbor lookup limited by 0032) |
| 0031 | [Robot names Harbor refuses, and what keeps truncated names apart](0031-robot-name-validity-and-truncation.md) | Accepted (amends 0018) |
| 0032 | [A bridge sets its finalizer right before the robot and releases only what it set](0032-finalizer-ownership.md) | Accepted (amends 0026, 0030; chart sets `BRIDGE_INSTANCE` without a selector since 0.12.3) |
| 0033 | [The installer rolls back a kubelet restart that does not verify](0033-installer-rollback-of-a-rejected-restart.md) | Accepted (refines 0021, 0029) |
| 0034 | [Patch mode edits the environment file the kubelet unit reads](0034-patch-mode-edits-the-environment-file-kubelet-reads.md) | Accepted (refines 0021) |
| 0035 | [Auto mode moves shared directories once every install is ready](0035-auto-mode-moves-shared-directories-when-every-install-is-ready.md) | Accepted (refines 0021, 0029) |
| 0036 | [Vulnerability checks fail a pull request for what it adds](0036-vulnerability-checks-fail-a-pull-request-for-what-it-adds.md) | Accepted (amends 0019; release gate amended by 0040) |
| 0037 | [The plugin exits non-zero when the bridge refuses](0037-plugin-exits-non-zero-on-a-refusal.md) | Accepted |
| 0038 | [The chart's values schema rejects unknown keys](0038-chart-values-schema-rejects-unknown-keys.md) | Accepted |
| 0039 | [Node-side uninstall](0039-node-side-uninstall.md) | Accepted (refines 0021, 0029, 0033, 0034, 0035) |
| 0040 | [A release stops on a called vulnerability that has a fix](0040-a-release-stops-on-a-called-vulnerability-with-a-fix.md) | Accepted (amends 0036) |
| 0041 | [The chart derives the audience and matchImages from the required values](0041-chart-derives-audience-and-match-images.md) | Accepted (refines 0021, 0026) |
| 0042 | [The chart creates a CA for the release when no issuer is named](0042-chart-creates-a-release-ca.md) | Accepted (refines 0008) |
| 0043 | [`trustPolicy.issuer` is optional and means the bridge's issuer](0043-trust-policy-issuer-is-optional.md) | Accepted (refines 0004, 0006) |

Format: [Michael Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions). Process: see ADR-0001.
