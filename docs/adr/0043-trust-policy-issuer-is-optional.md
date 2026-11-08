# 43. `trustPolicy.issuer` is optional and means the bridge's issuer

## Status

Accepted, 2026-11-08. Refines ADR-0004 (the trust policy as a CRD field)
and ADR-0006 (OIDC validation).

## Context

- Every HarborAccess names `spec.trustPolicy.issuer`, and it must equal the
  bridge's configured issuer (`BRIDGE_OIDC_ISSUER`, chart:
  `bridge.oidcIssuer`) byte for byte. Otherwise the bridge reports
  `IssuerMismatch` and serves nothing for the object.
- A bridge validates tokens of one issuer only: the data plane's Validator
  pins `iss` to the configured issuer and checks the signature against that
  issuer's keys (ADR-0006). The issuer in a HarborAccess therefore never
  widens what the bridge accepts. It can only make an object unusable.
- The value differs per platform (kind and kubeadm:
  `https://kubernetes.default.svc.cluster.local`; GKE, EKS and AKS: a
  provider URL), and a trailing slash or another spelling is a mismatch.
  Users copy it into every object, and a wrong copy is a common reason for
  an object that does not work.
- The issuer in an object still has a use: an object written for one
  cluster and applied to another one with another issuer is refused
  (`IssuerMismatch`) instead of served.
- The audience in an object is different. It decides which bridge serves
  the object when several bridges see it (ADR-0026), so it stays required.

## Decision

1. `spec.trustPolicy.issuer` is optional. An object without it trusts the
   issuer of the bridge that serves it. A set value must still equal the
   bridge's issuer, as before; the CRD still rejects a set value that is not
   an `http(s)://` URL, including the empty string.
2. The reconciler and the data plane resolve the issuer the same way
   (`TrustPolicy.IssuerOr` in the API package, which both may import). The
   data plane keeps its own check against the token's `iss` claim, now
   against the resolved issuer, and matches nothing when the resolved
   issuer is empty.

## Consequences

- A HarborAccess needs `serviceAccountRef`, `trustPolicy.audience` and
  `permissions`. The README and the samples leave the issuer out.
- An object without the issuer is served on any cluster whose bridge
  serves its audience and selector. Users who move objects between
  clusters and want them refused elsewhere keep setting the issuer.
- Helm does not upgrade CRDs from `crds/`. Until the new CRD is applied, the
  API server rejects an object without the issuer (`Required value`), which
  is the old behaviour. A bridge older than this change reports such an
  object as `IssuerMismatch` and serves nothing for it.

## Alternatives considered

- **Keep the issuer required.** Rejected: it never widens what the bridge
  accepts, and a wrong copy is a common failure.
- **Drop the field.** Rejected: objects that set it must keep working, and
  the refusal of objects applied to the wrong cluster is still useful to
  some users.
- **Make the audience optional too.** Rejected: see ADR-0041. Without it,
  every bridge whose selector matches an object would serve it.
