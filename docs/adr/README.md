# Architecture Decision Records

| #    | Title                                                                                  | Status   |
|------|----------------------------------------------------------------------------------------|----------|
| 0001 | [Record architecture decisions](0001-record-architecture-decisions.md)                 | Accepted |
| 0002 | [Bridge split into Control Plane and Data Plane](0002-bridge-control-plane-data-plane-split.md) | Accepted |
| 0003 | [Persistent Harbor robots per HarborAccess CR](0003-persistent-robots-per-harboraccess.md) | Accepted |
| 0004 | [trustPolicy as a first-class CRD field](0004-trust-policy-as-crd-field.md)            | Accepted |
| 0005 | [Docker token issuance via Harbor /service/token](0005-docker-token-via-service-token.md) | Superseded by 0013 |
| 0006 | [OIDC validation strategy and audience binding](0006-oidc-validation-and-audience.md)  | Accepted |
| 0007 | [Cache invalidation when HarborAccess CRs change](0007-cache-invalidation-on-cr-change.md) | Superseded by 0013 |
| 0008 | [Plugin-to-bridge transport: NodePort](0008-plugin-to-bridge-transport-nodeport.md)    | Accepted |
| 0009 | [Multi-cluster topology](0009-multi-cluster-topology.md)                               | Accepted (naming scheme + hyphen-prefix caveat superseded by 0018) |
| 0010 | [ServiceAccountRef as the canonical workload identity in HarborAccess](0010-service-account-ref-as-identity.md) | Accepted |
| 0011 | [Robot password storage as per-CR Kubernetes Secret in the bridge namespace](0011-robot-password-secret-storage.md) | Accepted |
| 0012 | [Robot description as the cross-component reconciler↔janitor contract](0012-robot-description-as-component-contract.md) | Accepted |
| 0013 | [Return robot Basic Auth credentials, not pre-minted Docker JWTs](0013-return-robot-basic-auth-credentials.md) | Accepted (supersedes 0005, supersedes 0007) |
| 0014 | [Handle Harbor's `robot$` prefix asymmetry at the comparison boundary](0014-harbor-robot-dollar-prefix-handling.md) | Accepted (normalization point moved into the Harbor client by 0023) |
| 0015 | [The plugin duplicates wire types instead of importing them](0015-plugin-duplicates-wire-types.md) | Accepted (enum-mismatch reasoning corrected by 0016) |
| 0016 | [`cacheKeyType` in the credential-provider response is `Registry`, not `ServiceAccount`](0016-credential-provider-cache-key-type.md) | Accepted (supersedes the enum claim in 0015) |
| 0017 | [The chart provisions audience-scoped RBAC for kubelet token requests](0017-chart-provisions-audience-rbac.md) | Accepted |
| 0018 | [Dot-delimited robot and Secret names for collision-free identity mapping](0018-dot-delimited-naming.md) | Accepted (supersedes 0009 naming scheme) |
| 0019 | [Code conventions and CI quality gates](0019-code-conventions-and-ci-quality-gates.md) | Accepted |
| 0020 | [Harbor compatibility is tested, not asserted](0020-harbor-compatibility-matrix.md) | Accepted |
| 0021 | [Node installer: Go binary with auto/merge/patch/none modes](0021-node-installer-modes.md) | Accepted |
| 0022 | [GKE e2e harness for merge-mode installation](0022-gke-e2e-harness.md) | Accepted (never run yet) |
| 0023 | [Level-triggered robot lifecycle, rotation-safe credential caching, and complete revocation](0023-level-triggered-robot-lifecycle.md) | Accepted (amends 0003, 0012, 0013, 0014) |
| 0024 | [The chart can leave the plugin to the platform; supported node platforms](0024-plugin-optional-and-platform-support.md) | Accepted (extends 0021) |
| 0025 | [Data plane serves on every replica; metrics leave the credential listener](0025-data-plane-serving.md) | Accepted (amends 0002, 0008) |
| 0026 | [One bridge serves one audience and a selected set of HarborAccess objects](0026-audience-pinning-and-harboraccess-selector.md) | Accepted (extends 0010, 0017, 0023) |

Format: [Michael Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions). Process: see ADR-0001.
