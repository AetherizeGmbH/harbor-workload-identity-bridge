# 28. Token lifetime cap and pod binding

## Status

Accepted (2026-09-27). Extends ADR-0006 (OIDC validation) and ADR-0010
(identity). Closes threat-model item O3.

## Context

- The data plane validates the pod's ServiceAccount token locally against
  the apiserver's JWKS (ADR-0006; `bridge/dataplane/oidc.go`,
  `handler.go:189`, where only `ForceLocalValidation` is implemented).
  go-oidc rejects expired tokens; beyond that the validator checked only
  `sub`, `aud` and `iss`. It already read the pod name, pod UID and node
  from the `kubernetes.io` claim, for the audit log only.
- Any valid token of the served audience was therefore accepted until its
  `exp`, whatever lifetime its issuer chose.
  `kubectl create token <sa> --audience=<aud> --duration=8760h` yields a
  one-year token for anyone allowed to create tokens for that
  ServiceAccount, and tokens not bound to a pod were accepted as well.
- Local validation never sees the bound object. Only the apiserver's
  TokenReview checks that a token's pod (or Secret, or node) still exists:
  "The API server's TokenReview endpoint will validate the
  BoundObjectRef, but other audiences may not. Keep ExpirationSeconds
  small if you want prompt revocation." (k8s.io/api v0.37.1,
  `authentication/v1/types.go:216-219`). A pod-bound token stays valid at
  the bridge after its pod or its ServiceAccount is deleted, until `exp`.
- The tokens kubelet sends are short and pod-bound. Line references are
  to kubernetes/kubernetes v1.37.1:
  - The credential provider's TokenRequest sets no `expirationSeconds`
    and binds the token to the pod
    (`pkg/credentialprovider/plugin/plugin.go:342-356`).
  - The TokenRequest default is 3600 seconds
    (`pkg/apis/authentication/v1/defaults.go:28-33`).
    `--service-account-max-token-expiration` must be at least one hour
    (`pkg/controlplane/apiserver/options/options.go:293-301`), so it never
    shortens that default; only an external token signer can cap tokens
    lower (same file, 316-335). The apiserver writes `iat`, `nbf` and
    `exp` from the same instant (`pkg/serviceaccount/claims.go:82-84`), so
    `exp - iat` is exactly the requested lifetime.
  - The apiserver's legacy expiration extension applies only to exactly
    3607 seconds and kube audiences
    (`pkg/registry/core/serviceaccount/storage/token.go:286`), so never
    to the bridge's audience.
  - Kubelet takes these tokens from its token manager
    (`pkg/kubelet/kubelet.go:789,841`), which refreshes a token older than
    80% of its lifetime (`pkg/kubelet/token/token_manager.go:172-187`).
- Checked on a kind cluster (Kubernetes v1.37.0, 2026-09-27): a token from
  `kubectl create token … --duration=1h --bound-object-kind=Pod` has
  `exp - iat = 3600` and `kubernetes.io.pod.{name,uid}`; the apiserver
  issued an 8760h token on request; binding a token to a pod that runs as
  another ServiceAccount fails. Through this validator the pod-bound
  token passes, the unbound one is `not_pod_bound` and the 8760h one
  `excessive_lifetime`. The e2e stage `token_rejection`
  (`test/e2e/tests/02-bridge.tftest.hcl`) repeats this on every run
  against the deployed bridge, with tokens from the TokenRequest API: a
  pod-bound 1h token gets credentials, an unbound 1h token and a
  pod-bound 2h token get `401`, and the audit log shows both categories.
- Identity is the ServiceAccount (ADR-0010). Many pods may share one
  ServiceAccount (GitLab runners, for example), and the chart sets
  `cacheType: ServiceAccount`, so kubelet caches credentials per
  ServiceAccount. Pod binding changes nothing about who may pull.

## Decision

1. **Lifetime cap.** The validator rejects a token whose lifetime
   `exp - iat` exceeds `BRIDGE_TOKEN_MAX_LIFETIME` (a Go duration, default
   `1h`; chart: `bridge.tokenValidation.maxLifetime`). Kubelet's tokens
   have exactly 3600 seconds and pass. A token without `iat` is rejected,
   because its lifetime cannot be determined. A token whose `iat` lies
   more than five minutes in the future is rejected too: it could stay
   valid for longer than the cap from now. Five minutes is the clock skew
   go-oidc already allows for `nbf` (go-oidc v3.21.0,
   `oidc/verify.go:273-282`), which Kubernetes sets to `iat`, so no token
   kubelet sends is affected. The value must be positive; the
   bridge refuses to start otherwise, and the chart fails at template
   time.
2. **Pod binding.** The validator rejects a token without the
   `kubernetes.io` pod claim (pod name and UID).
   `BRIDGE_REQUIRE_POD_BOUND_TOKEN` (default `true`; chart:
   `bridge.tokenValidation.requirePodBinding`) turns this off. `false` is
   an escape hatch for hand-minted tokens in local development, it weakens
   the bridge, and the bridge and the chart NOTES say so.
3. **In the validator.** Both checks live in `Validator.Validate`, so
   every caller gets them. A rejected token is a `401` like any other
   invalid token. The audit line keeps `reason=invalid_token` and gains a
   `category` field with the validation-failure category, which is also
   the label of `bridge_oidc_validation_failures_total`. The new
   categories are `excessive_lifetime` and `not_pod_bound`; the checks
   return sentinel errors, so they are classified without matching error
   text. The token is never logged.
4. **No TokenReview.** No apiserver call per request and no new RBAC
   (ADR-0006 stands).

## Consequences

- A hand-minted token must be bound to a pod that runs as the
  ServiceAccount and last no longer than the cap:
  `kubectl create token <sa> -n <ns> --audience=<aud> --duration=1h
  --bound-object-kind=Pod --bound-object-name=<pod>`. The apiserver
  refuses to bind a token to a pod that runs as another ServiceAccount.
  MIGRATION.md and HOW-TO-TEST.md show the command.
- The cap bounds how long one token can be redeemed at the bridge, not
  how long the credentials it buys stay valid. The bridge returns the
  robot's password, which Harbor accepts until the next rotation. The
  bridge schedules that rotation `PasswordRotationInterval` (24h) plus
  `RotationSafetyMargin` (1m) after the previous one
  (`bridge/controlplane/contract.go`; ADR-0003, ADR-0023); deleting the
  robot's password Secret rotates at once, deleting the HarborAccess
  deletes the robot.
- Whoever may create tokens for a ServiceAccount can still mint a
  pod-bound token for any existing pod of that ServiceAccount. One such
  token per rotation keeps them supplied with credentials, so sustained
  abuse costs about one TokenRequest a day in the apiserver audit log,
  not one an hour. Every accepted token now names a pod, so every audit
  line carries `pod` and `pod_uid`.
- Residual risk: a stolen kubelet token stays redeemable at the bridge
  for its remaining lifetime, at most one hour, after its pod or its
  ServiceAccount is gone, and a password redeemed in that hour works at
  Harbor until the next rotation, up to about 24 hours later. Only
  TokenReview would close the first window, at the cost of an apiserver
  round trip per pull; deleting the password Secret closes the second.
- A cap below one hour refuses every kubelet token, and every pull
  through the bridge fails, unless an external token signer issues
  shorter tokens. The chart and SECURITY.md document this; the bridge
  logs it at startup.
- The pod claim now takes part in a trust decision (it must be present).
  Which pod it names still does not: authorization stays with the
  ServiceAccount.
