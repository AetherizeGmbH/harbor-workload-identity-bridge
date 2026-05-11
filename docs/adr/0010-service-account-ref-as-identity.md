# 10. ServiceAccountRef as the canonical workload identity in HarborAccess

## Status

Accepted

## Context

The original `HarborAccess` CRD (v1alpha1, [ADR-0004](0004-trust-policy-as-crd-field.md)) carried workload identity only inside `spec.trustPolicy.subjectMatch` — a string of the form `system:serviceaccount:<namespace>:<name>`. The reconciler needs the SA's `namespace` and `name` separately to construct the per-cluster Harbor robot name (`bridge-<cluster>-<ns>-<sa>`, [ADR-0009](0009-multi-cluster-topology.md)).

Three options for getting them:

1. **Parse `trustPolicy.subjectMatch`** with a regex. Zero CRD churn. Brittle: any future change to `subjectMatch` (wildcards, claim matchers, non-SA subjects) breaks robot naming.
2. **Add an explicit `spec.serviceAccountRef.{namespace,name}` field.** One field-shape change to v1alpha1, additive. Robot naming decouples from claim shape. Future trust-policy generalisation does not touch identity.
3. **Use the HarborAccess CR's own `metadata.{namespace,name}`.** No new field. Loses connection between robot name and SA identity; operators have to look at the CR to see which SA can use it.

## Decision

Add explicit `spec.serviceAccountRef.{namespace,name}` to `HarborAccess` and make it the **single source of truth** for workload identity in v1alpha1. Both fields are required, validated as DNS-1123 labels.

`trustPolicy.subjectMatch` is **removed**. The bridge derives the expected `sub` claim from `serviceAccountRef` in canonical Kubernetes form: `system:serviceaccount:<namespace>:<name>` ([ADR-0006](0006-oidc-validation-and-audience.md)). One field expresses the identity; both the control plane (robot naming) and the data plane (token validation) consume it.

## Consequences

- One source of truth. Operators write the identity once; the data plane derives the expected subject internally.
- Robot naming and password rotation are driven by stable, explicit fields. [ADR-0009](0009-multi-cluster-topology.md)'s ownership-prefix invariant rests on a deterministic, non-derived input.
- This is a breaking change inside v1alpha1: tooling that emitted `HarborAccess` manifests with `subjectMatch` must remove the field and add `serviceAccountRef`. v1alpha1 is alpha; no backward-compatibility commitment was made.
- Future generalisation of trust policy (wildcards, claim matchers, non-Kubernetes token sources) lands as additive fields on `trustPolicy` in a future API version. The shape change is contained to that version; v1alpha1 stays simple.

## Alternatives considered

- **Parse `trustPolicy.subjectMatch`.** Rejected. The robot's name would be derived from a field whose grammar we intend to relax in v1alpha2 (claim matchers, wildcard subjects). That couples a future feature to a security-critical naming invariant.
- **Use `metadata.{namespace,name}` of the HarborAccess CR.** Rejected: the example in [the multi-cluster spec](0009-multi-cluster-topology.md) makes clear the operator-facing audit value is "which SA is this robot for", not "which CR is this robot for". Robot names should reflect SA identity.
- **Keep both `serviceAccountRef` and `subjectMatch`, with a CEL rule forcing equality.** Initially adopted, then rejected: the two fields encoded identical information, and a CEL "must be equal" rule paperied over the redundancy rather than fixing it. The operator-facing duplication has no benefit in v1alpha1, which supports only Kubernetes-issued SA tokens with the canonical subject format.
