# 7. Cache invalidation when HarborAccess CRs change

## Status

**Superseded by [ADR-0013](0013-return-robot-basic-auth-credentials.md).** This ADR governed the data-plane docker-token cache that ADR-0005 introduced. ADR-0013 removed the cache (the data plane no longer mints JWTs), so there is nothing left to invalidate. The principles below (generation-keyed lazy invalidation, lazy over active eviction, single-process shared informer) remain applicable to any future cache the bridge adds.

---

(Originally Accepted)

## Context

The Data Plane caches minted docker JWTs in `ttlcache` to avoid hammering Harbor's `/service/token` on every kubelet request. That cache must invalidate when the `HarborAccess` CR changes (permissions edit, trustPolicy edit), otherwise a workload could keep using a token granting old permissions.

The Control Plane and Data Plane run in one process ([ADR-0002](0002-bridge-control-plane-data-plane-split.md)), so they can share state cheaply. Two viable invalidation strategies:

1. **Active invalidation.** The Reconciler emits an event when a CR's generation changes; the Data Plane subscribes and evicts matching cache entries.
2. **Lazy invalidation via generation-keyed cache.** The cache key includes the CR's `metadata.generation`. When the CR changes, subsequent lookups use the new generation and miss naturally. Old entries expire on TTL.

## Decision

Lazy invalidation. The Data Plane cache key is

    (harborAccess namespace, harborAccess name, harborAccess generation, requesting SA subject)

The Data Plane reads `HarborAccess` objects through controller-runtime's informer cache (shared with the Control Plane). On each request the Data Plane:

1. Looks up the matching `HarborAccess` from the informer cache.
2. Builds the cache key including the current `generation`.
3. Cache hit returns the JWT. Cache miss mints a new JWT via `/service/token` and stores under the new key.

Old entries are not actively evicted; they age out on TTL.

Robot-password rotation does NOT bump CR generation, so it cannot rely on this mechanism. The Data Plane treats a `/service/token` 401 as the signal that its in-memory robot password is stale, re-reads the Kubernetes Secret, and retries once.

## Consequences

- No cross-package signalling. `dataplane` does not import `controlplane` ([ADR-0002](0002-bridge-control-plane-data-plane-split.md) holds).
- A bounded window exists where a request that arrived during reconciliation may use an entry minted under the old generation. The window is bounded by the cached token's TTL (`≤ spec.tokenTTL`, max 24h) and the workload's re-request rate. Both are operator-controlled.
- Cache memory grows with `(generations × subjects)`. The `ttlcache` is sized with an entry cap and per-entry TTL equal to `spec.tokenTTL`.

## Alternatives considered

- **Active invalidation via Go channels.** Rejected: introduces an intra-process API surface between two packages we want to remain independent.
- **Cache-aside with explicit eviction calls from the Reconciler.** Rejected for the same coupling reason.
- **No cache at all.** Rejected: every pod start would hit Harbor's `/service/token`, a single Harbor process and known bottleneck.
