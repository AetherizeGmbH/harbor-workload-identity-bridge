# 44. The bridge reads the issuer from the apiserver when none is set

## Status

Accepted, 2026-11-08. Refines ADR-0006 (OIDC validation) for the chart's
default.

## Context

- The bridge validates the ServiceAccount tokens of one issuer:
  `BRIDGE_OIDC_ISSUER` (chart: `bridge.oidcIssuer`). It must equal the
  `iss` claim of kubelet's tokens byte for byte, or every token is refused.
  The chart's default, `https://kubernetes.default.svc.cluster.local`, is
  the issuer kubeadm and kind set.
- Managed clusters use other issuers: GKE
  `https://container.googleapis.com/v1/projects/P/locations/L/clusters/C`,
  EKS `https://oidc.eks.<region>.amazonaws.com/id/<ID>`, AKS with its OIDC
  issuer `https://<region>.oic.prod-aks.azure.com/<tenant>/<uuid>/` (with
  the trailing slash). Users had to find the value and set it. They also
  had to set `bridge.oidcJWKSURL=https://kubernetes.default.svc/openid/v1/jwks`:
  without it the bridge runs discovery at the cloud issuer, which needs
  internet egress and fails TLS verification with the chart's default
  `bridge.oidcCAFile` (the cluster CA is the only trust root).
- The apiserver serves its discovery document at
  `/.well-known/openid-configuration` and its keys at `/openid/v1/jwks`
  (ServiceAccountIssuerDiscovery, stable since Kubernetes 1.21). The
  document's `issuer` is the first `--service-account-issuer`, the one new
  tokens carry ("the first is used to generate tokens and all are used to
  determine which issuers are accepted", kube-apiserver flag help;
  `pkg/controlplane/apiserver/config.go` line 426 in Kubernetes v1.37.2).
  Its `jwks_uri` is the apiserver's external address, or
  `--service-account-jwks-uri`; the apiserver serves the keys at
  `/openid/v1/jwks` either way. It serves both endpoints only when the
  issuer is an https URL without query and fragment
  (`pkg/serviceaccount/openidmetadata.go`, lines 127-139).
- The bridge already fetches the keys from the in-cluster apiserver, with
  the cluster CA and its own token, and its ClusterRole allows `get` on
  both URLs since 0.12.2 (ADR-0006 status).
- go-oidc's `NewProvider` refuses a document whose `issuer` is not the URL
  it fetched the document from (go-oidc v3.21.0, `oidc/oidc.go` line 331).
  Fetched from `https://kubernetes.default.svc`, the issuer of every
  managed cluster differs. `InsecureIssuerURLContext` lifts the check only
  for an issuer the caller already knows.

## Decision

1. An empty `BRIDGE_OIDC_ISSUER` means: at startup, before either plane
   starts, the bridge reads
   `https://kubernetes.default.svc/.well-known/openid-configuration` and
   uses the document's `issuer`. It reads it with its OIDC client: the
   cluster CA (`BRIDGE_OIDC_CA_FILE`), its own token (`BRIDGE_OIDC_TOKEN_FILE`,
   sent only to the in-cluster apiserver), no redirects, at most 30 s and
   64 KiB. The bridge reads the document itself, not with `NewProvider`.
2. The issuer in the document must be an https URL with a host, without a
   `user:password@` part, an `@`, a query or a fragment, and must read back
   unchanged after parsing, so that the bridge compares exactly what the
   apiserver writes into its tokens. The bridge exits otherwise.
3. With a read issuer and no `BRIDGE_OIDC_JWKS_URL`, the keys come from
   `https://kubernetes.default.svc/openid/v1/jwks`, not from the document's
   `jwks_uri` (on managed clusters an internet URL). A set
   `BRIDGE_OIDC_JWKS_URL` still names where the keys come from. Either way
   the accepted algorithms are the document's
   `id_token_signing_alg_values_supported` that the key set can verify
   (RS256 when it names none of them), as with discovery at a set issuer.
   Kubernetes derives that list from its keys' types
   (`pkg/serviceaccount/openidmetadata.go`, `getAlgs`).
4. The read issuer is the bridge's issuer: it fills `Config.OIDCIssuer`,
   so the validator, the reconciler's `IssuerMismatch` check and the data
   plane use the same value, as they used the configured one. The startup
   log shows it.
5. A set `BRIDGE_OIDC_ISSUER` works as before. It is needed off-cluster
   and when the apiserver does not serve the document.
6. When the bridge cannot read the issuer, it exits with an error that
   names the URL, the RBAC rule and `bridge.oidcIssuer`. It does not fall
   back to a default.
7. The chart's `bridge.oidcIssuer` defaults to `""` and renders
   `BRIDGE_OIDC_ISSUER` only when set. The chart checks a set value as
   before.

## Consequences

- A first install on GKE, EKS or AKS needs neither `bridge.oidcIssuer` nor
  `bridge.oidcJWKSURL`, provided the apiserver serves the discovery
  document in the cluster. Kubernetes does whenever its issuer is an https
  URL. The providers do not document the in-cluster endpoints; the GKE
  harness (`test/e2e-gke`) exercises them but has not run yet.
- The issuer comes from the source the keys already came from: the
  in-cluster apiserver over TLS, verified with the cluster CA. Whoever can
  change the document there can already change the keys, so this adds no
  trust anchor. The `iss` check still separates only clusters whose issuers
  differ (SECURITY.md).
- On kind and kubeadm the read issuer is the old default, so nothing
  changes there. Elsewhere a HarborAccess must name the read issuer, or it
  is refused (`IssuerMismatch`).
  `kubectl get --raw /.well-known/openid-configuration | jq -r .issuer`
  prints it, and the bridge logs it at startup.
- The bridge reads the issuer once. After the cluster's issuer changes,
  it uses the new one from its next start; before, the change needed a new
  value and a `helm upgrade`. With several `--service-account-issuer`
  values the bridge accepts tokens of the first only, as with a configured
  issuer.
- Off-cluster (`make run-local`) `kubernetes.default.svc` does not resolve;
  the Makefile still requires `BRIDGE_OIDC_ISSUER`.
- `helm upgrade --reuse-values` renders with the previous chart's values
  (Helm v4.2.3, `pkg/action/upgrade.go` lines 617-631), so a release that
  never set `bridge.oidcIssuer` keeps
  `https://kubernetes.default.svc.cluster.local` until the value is set to
  `""`. An upgrade without `--reuse-values`, or with
  `--reset-then-reuse-values`, gets the new default.

## Alternatives considered

- **Keep the kind default and document each platform.** Rejected: users
  must find the value, and a wrong copy (a missing trailing slash on AKS)
  refuses every token.
- **go-oidc's `NewProvider`.** Rejected: it refuses the managed clusters'
  documents (see Context).
- **Read the issuer from the bridge's own projected token.** Rejected: the
  bridge would trust an unverified JWT payload, the document is the
  interface Kubernetes publishes for relying parties, and the bridge needs
  the document's algorithms anyway.
- **Ask the apiserver through the controller-runtime client** (`rest.Config`,
  which also works off-cluster with a kubeconfig). Rejected: a second HTTP
  path with other credentials for one trust source. The OIDC client already
  carries the rules for the bridge's token (only to the apiserver, no
  redirects, bounded), and off-cluster runs set the issuer anyway.
- **Look the issuer up in the chart.** Rejected: Helm's `lookup` reads
  only resources, not `/.well-known/openid-configuration`, and `helm
  template` has no cluster.
