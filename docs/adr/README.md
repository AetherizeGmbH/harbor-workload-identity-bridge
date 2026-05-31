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
| 0009 | [Multi-cluster topology](0009-multi-cluster-topology.md)                               | Accepted |
| 0010 | [ServiceAccountRef as the canonical workload identity in HarborAccess](0010-service-account-ref-as-identity.md) | Accepted |
| 0011 | [Robot password storage as per-CR Kubernetes Secret in the bridge namespace](0011-robot-password-secret-storage.md) | Accepted |
| 0012 | [Robot description as the cross-component reconciler↔janitor contract](0012-robot-description-as-component-contract.md) | Accepted |
| 0013 | [Return robot Basic Auth credentials, not pre-minted Docker JWTs](0013-return-robot-basic-auth-credentials.md) | Accepted (supersedes 0005, supersedes 0007) |
| 0014 | [Handle Harbor's `robot$` prefix asymmetry at the comparison boundary](0014-harbor-robot-dollar-prefix-handling.md) | Accepted |
| 0015 | [The plugin duplicates wire types instead of importing them](0015-plugin-duplicates-wire-types.md) | Accepted |

Format: [Michael Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions). Process: see ADR-0001.
