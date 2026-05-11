# 6. OIDC validation strategy and audience binding

## Status

Accepted

## Context

The Data Plane validates the service-account token presented by the kubelet credential-provider plugin. Two viable approaches:

1. **Kubernetes `TokenReview` API.** POST the token to the apiserver; the apiserver verifies and returns the authenticated user. Idiomatic for in-cluster validators.
2. **OIDC JWKS validation via `go-oidc/v3`.** Discover JWKS at the cluster's service-account issuer (e.g. `https://kubernetes.default.svc`), cache and rotate keys locally, validate signatures in-process.

We also need to bind the token's `aud` claim, so a token issued for some other purpose cannot be replayed against the bridge.

## Decision

The bridge validates SA tokens with `github.com/coreos/go-oidc/v3` against the cluster's service-account issuer, with mandatory audience binding.

- Issuer is configured via Helm value (default `https://kubernetes.default.svc`).
- JWKS discovery uses the `system:service-account-issuer-discovery` ClusterRole. The Helm chart binds this role to the bridge ServiceAccount.
- Audience is configured per `HarborAccess.spec.trustPolicy.audience` and must match the kubelet credential-provider config's `serviceAccountTokenAudience` for the registry.
- Subject is **derived** from `HarborAccess.spec.serviceAccountRef.{namespace,name}` in the canonical Kubernetes form: `system:serviceaccount:<namespace>:<name>`. v1alpha1 supports only this exact-match shape; wildcards and claim matchers are forward-compatible additions for a future API version (see [ADR-0010](0010-service-account-ref-as-identity.md) for why we removed the duplicated `trustPolicy.subjectMatch` field).

The kubelet credential-provider config (installed by the Helm chart) sets `cacheType: ServiceAccount` (new KEP-4412 beta field, required in 1.34+). Rationale: docker tokens are issued per-SA (one robot per `HarborAccess`, one CR per SA-subject) and are not bound to the SA-token's specific lifetime, so caching by SA identity is correct. `Token` cache type would discard valid docker tokens every time the kubelet re-projects the SA token.

## Consequences

- Validation does not hit the apiserver per request. `TokenReview` would centralise validation but add an apiserver round-trip on every image pull. With go-oidc and local JWKS cache, validation is a CPU operation.
- We rely on the cluster exposing OIDC discovery endpoints (default since Kubernetes 1.21). Clusters with custom auth configurations override the issuer via Helm value.
- Audience binding is enforced at the CR level: each `HarborAccess` declares the audience it accepts; the bridge rejects any token whose `aud` claim does not include it. Misconfigured kubelet plugins fail loudly.
- `cacheType: ServiceAccount` means SA revocation takes effect at most when the cached docker token expires. We document this in `SECURITY.md` and recommend short `spec.tokenTTL` for sensitive workloads.

## Alternatives considered

- **TokenReview-based validation.** Rejected for latency and apiserver-load reasons above. We may revisit if go-oidc encounters issuer-discovery problems in specific distros.
- **`cacheType: Token`.** Rejected: tightly couples our docker-token TTL to kubelet's SA-token projection cadence (10 minutes by default), which would force us either to keep refreshing docker tokens needlessly or accept a much shorter effective TTL than `spec.tokenTTL`.
