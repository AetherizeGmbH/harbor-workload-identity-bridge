# 4. trustPolicy as a first-class CRD field

## Status

Accepted

## Context

The eventual upstream Harbor design ([goharbor/harbor#17520][upstream]) will attach an OIDC Trust Policy directly to a Harbor robot (issuer, audience, subject match). Today no such Harbor field exists, so the bridge's Data Plane must enforce the policy itself before minting docker tokens.

We had to decide whether `trustPolicy`:

1. lives on the `HarborAccess` CRD itself, visible to operators today and stable across the migration, or
2. lives in a separate object or annotation kept out of the CRD until Harbor lands the upstream feature.

## Decision

`trustPolicy` is a first-class field on `HarborAccess.spec`. It is present from day one and has the same shape we expect Harbor's eventual API to require: `issuer`, `audience`, with room for `claimMatchers` and similar additive policy fields later. (The subject under policy is derived from `spec.serviceAccountRef` — see [ADR-0010](0010-service-account-ref-as-identity.md). Earlier drafts of this ADR named a `subjectMatch` field inside `trustPolicy`; that field was removed in favour of `serviceAccountRef` as the single source of truth for workload identity.)

`HarborAccess.status` carries `trustPolicyEnforcedBy: bridge | harbor` so operators can see which component is currently enforcing.

## Consequences

- The CRD does not change shape at migration time. Operators do not rewrite their `HarborAccess` manifests; the migration is a reconciler-internal change plus a Helm value flip.
- Day-one users write a `trustPolicy` block even though no Harbor feature backs it — this is correct, because the bridge enforces it, not Harbor.
- Validation of `trustPolicy.issuer` and `trustPolicy.audience` happens via kubebuilder markers on the CRD; bad input is rejected by the apiserver.
- `claimMatchers` is reserved as a forward-compatible empty slot. Future fields are additive (no breaking version bumps for additions).

## Alternatives considered

- **Annotations until Harbor catches up.** Rejected: makes the migration a breaking change for every operator. Loses CRD validation, requiring the bridge to validate at runtime.
- **A separate `HarborTrustPolicy` CRD.** Rejected: forces two correlated CRs per workload, doubles RBAC surface, and produces no benefit since trust policy is meaningless without permissions.

[upstream]: https://github.com/goharbor/harbor/issues/17520
