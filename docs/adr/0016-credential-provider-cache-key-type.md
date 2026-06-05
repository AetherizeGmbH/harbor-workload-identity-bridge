# 16. `cacheKeyType` in the credential-provider response is `Registry`, not `ServiceAccount`

## Status

Accepted. Supersedes the implicit claim about the `cacheKeyType` enum
made in [ADR-0015](0015-plugin-duplicates-wire-types.md) §"Enum mismatch
on cacheKeyType".

## Context

The kubelet image-credential-provider API has two superficially similar
fields whose names invite confusion:

1. **`tokenAttributes.cacheType`** — set by the cluster operator in
   `CredentialProviderConfig`. Controls how kubelet keys its
   *SA-token cache*. Valid values under KEP-4412: `Token`,
   `ServiceAccount`. We use `ServiceAccount` per
   [ADR-0006](0006-oidc-validation-and-audience.md).
2. **`CredentialProviderResponse.cacheKeyType`** — emitted by the
   plugin. Controls how kubelet keys its *credential cache* (the
   docker auth map). Valid values per the upstream
   `PluginCacheKeyType` enum: `Image`, `Registry`, `Global`.
   **`ServiceAccount` is not a member.**

These two are routinely conflated because both have "cache" + "type"
in the name and both appear in KEP-4412 discussions. The original
bridge code (`bridge/dataplane/handler.go`) emitted
`cache_key_type: "ServiceAccount"` in every `/v1/credentials`
response, reasoning that since kubelet caches the SA token by
ServiceAccount upstream, the credential should also be keyed by SA.
ADR-0015 even cited this as a reason for the plugin to use raw
strings instead of importing the upstream enum.

The actual behavior: kubelet validates `cacheKeyType` against
`PluginCacheKeyType` and rejects anything outside `{Image, Registry,
Global}` with
`credential provider plugin did not return a valid cacheKeyType: ServiceAccount`.
The credential is then discarded, kubelet retries with no credentials,
and containerd pulls anonymously — which a private Harbor refuses with
`no basic auth credentials`. The credential provider runs successfully
end-to-end, the network/auth chain is intact, and the only thing
visible to the operator is `ImagePullBackOff` with no actionable
upstream signal.

Confirmed by e2e testing in 2026-06: with `ServiceAccount` the pull
fails 100% of the time; with `Registry` it succeeds. The bug shipped
under v0.0.x and v0.1.x — every release before the fix was
non-functional in a `ServiceAccountNodeAudienceRestriction`-enabled
cluster (default since v1.32).

## Decision

The bridge emits `cache_key_type: "Registry"` in every successful
`/v1/credentials` response. The plugin passes it through verbatim to
kubelet as `cacheKeyType`.

`Registry` is the right choice for the per-`HarborAccess` credential
model:

- One `HarborAccess` CR ⇒ one Harbor robot.
- One Harbor robot ⇒ permissions across one Harbor project (typically
  multiple repositories under one registry host).
- Therefore: kubelet should re-use the credential across every repo
  on the same registry host. That's exactly `Registry`-keyed caching.
- `Image` would force a fresh plugin invocation for every distinct
  image:tag, defeating the cache and increasing bridge load
  proportionally to the number of distinct images pulled.
- `Global` would re-use one credential across registries, which is
  wrong as soon as a cluster has more than one Harbor.

`ServiceAccount`-style "one cache entry per SA" is already provided by
kubelet's own SA-token cache (`tokenAttributes.cacheType: ServiceAccount`)
at the *level above* this one. The plugin is invoked at most once per
unique (SA, registry) pair within a token's lifetime; the response's
`cacheKeyType` controls only how the *resulting credentials* are
re-used across image pulls within that token's lifetime.

## Consequences

- A credential issued for SA `A` accessing `harbor.example.com/project/a`
  is re-used by the same kubelet for SA `A` accessing
  `harbor.example.com/project/b` until expiry. Since the bridge issues
  one robot per `HarborAccess` and a HarborAccess covers one project,
  cross-project pulls within the same registry will still see one
  robot's credentials cached. If that robot doesn't have the
  necessary permission on project `b`, Harbor returns 401 and kubelet
  evicts the cache entry — correct fail-closed behavior.
- Multi-cluster topologies that share a bridge but have distinct
  Harbors per cluster are unaffected: each cluster's kubelets cache
  independently, and each registry host is its own cache key.
- The plugin's `cacheKeyType` test fixtures that used `"ServiceAccount"`
  as a synthetic pass-through value were misleading — they perpetuated
  the same confusion. Updated to `"Image"` (still synthetic, but
  obviously distinct from the bridge's `"Registry"` so the
  pass-through assertion still has signal).

### Update to ADR-0015

ADR-0015's first bullet under "Two problems with importing upstream
`k8s.io/kubelet`" — the enum-mismatch argument — was built on the
false premise that `ServiceAccount` is a `PluginCacheKeyType` member
in some kubelet module version. It isn't, and never was. The
*conclusion* of ADR-0015 (the plugin duplicates wire types) is
correct on the OTHER grounds in that ADR (transitive dep weight,
data-plane / control-plane separation per ADR-0002), so the ADR stays
Accepted. This ADR replaces the specific enum-mismatch justification.

## Alternatives considered

- **`cacheKeyType: Image`.** Rejected — would force a plugin
  invocation per distinct image tag, multiplying bridge load and
  defeating the purpose of credential caching in a cluster that pulls
  many distinct images from the same project.
- **`cacheKeyType: Global`.** Rejected — incorrect for multi-Harbor
  clusters, and even in single-Harbor setups it would re-use a robot
  with project-`A` permissions when pulling from project `B`. Fail-
  closed Harbor behavior would still surface the mismatch, but with
  more cache thrash than `Registry`.
- **Stop emitting `cacheKeyType` and let kubelet apply a default.**
  Rejected — kubelet's default for the field is implementation-defined
  and has flipped between releases. Explicit is better.
- **Validate the emitted value against an enum at the bridge boundary
  so future renames surface as test failures.** Adopted via the test
  fixture rewrite above; the bridge constant
  (`cacheKeyTypeRegistry = "Registry"`) is unit-tested as the literal
  string against the response shape.
