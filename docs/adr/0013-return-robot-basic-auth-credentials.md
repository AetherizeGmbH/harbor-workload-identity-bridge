# 13. Return robot Basic Auth credentials, not pre-minted Docker JWTs

## Status

Accepted. Supersedes [ADR-0005](0005-docker-token-via-service-token.md). Renders [ADR-0007](0007-cache-invalidation-on-cr-change.md) inert in v0.1 (no docker-token cache to invalidate).

**Empirically validated 2026-05-31** against kind + Harbor 2.x. The full credential-issuance path was driven from a real Kubernetes SA token through the bridge to a successful `crane pull` of an Alpine image from Harbor (4MB OCI tarball with config blob + layer + manifest). `crane` exercises the same `401 → Bearer realm → POST /service/token → JWT → /v2/ pull` handshake containerd uses, so the architectural claim in this ADR — that the bridge can hand off raw robot Basic Auth credentials and let the registry client complete the handshake itself — is no longer hypothesis. See [HOW-TO-TEST.md](../../HOW-TO-TEST.md) for the reproducible procedure and the captured audit-log + metrics output.

## Context

[ADR-0005](0005-docker-token-via-service-token.md) committed to the bridge minting Docker registry v2 bearer JWTs via Harbor's `/service/token` endpoint using the robot's password, then returning the JWT as the credential's `Password`. The rationale was security: the robot password never leaves the bridge process, only short-lived JWTs do.

While implementing the data-plane handler ([Phase 3 Slice C, commit 2d43fde](../../bridge/dataplane/handler.go)) we noted in code and commit that the approach hinged on containerd accepting the JWT as a bearer credential, and that this needed e2e validation. Further analysis of the kubelet → containerd → Harbor auth flow makes clear the approach **cannot work** with stock components:

1. The kubelet `CredentialProviderResponse` carries `{Username, Password}`. Kubelet writes these into a Docker-style `config.json` for containerd.
2. Containerd treats `{Username, Password}` as HTTP **Basic Auth** credentials.
3. Containerd's pull flow is: request → registry replies `401 WWW-Authenticate: Bearer realm=…service=…scope=…` → containerd POSTs to that realm with the credentials as Basic Auth and the requested scope as a query param → registry returns a scoped JWT → containerd uses that JWT in subsequent calls.
4. If we put a JWT into `Password`, step 3 sends `Authorization: Basic base64("<token>":<JWT>)` to Harbor's `/service/token`. Harbor looks up a user named `<token>`, does not find it, returns 401. The pull fails.
5. KEP-4412 has no "this is a bearer credential" channel — `CredentialProviderResponse` cannot signal "skip the auth handshake, use this as the JWT directly". Every credential-provider plugin (ECR, GCR, ACR) returns Basic-Auth-shaped credentials for this reason.

ADR-0005's intent (robot password never leaves the bridge) is architecturally unattainable without changes to either KEP-4412 or containerd that we cannot make.

## Decision

The data plane returns the robot's actual Basic Auth credentials in the `CredentialProviderResponse`:

```json
{
  "username": "robot$bridge-<cluster>-<ns>-<sa>",
  "password": "<robot password from the bridge-namespace Secret>",
  "expires_in": <spec.tokenTTL seconds>,
  "cache_key_type": "ServiceAccount"
}
```

Containerd performs the `/service/token` auth handshake itself; the bridge never calls `/service/token`. This matches how ECR, GCR, ACR, and `harbor-workload-identity-federation` (the named reference impl) all work in practice.

Code consequences:

- The handler reads the robot Secret and writes its contents into the response. No `/service/token` call.
- `bridge/dataplane/harbor_token.go` and `bridge/dataplane/cache.go` are removed: nothing in the data plane mints JWTs or caches them.
- The 401-retry path in the handler is removed: no `/service/token` call, no 401 to handle here.
- The `Validator` and `findHarborAccess` logic (SA-token validation, sub/aud routing) is unchanged.
- `bridge/controlplane/harbor` (the SDK wrapper used by the reconciler) is unchanged. The reconciler still creates/updates/deletes robots via Harbor's REST API.

## Consequences

### Security

The robot password reaches containerd's memory during image pulls (just as it does in ECR/GCR/ACR plugin designs). The bridge no longer holds the "password never leaves" guarantee, but it preserves the substantive ones:

- **Workloads cannot read the Secret directly.** It lives in the bridge namespace ([ADR-0011](0011-robot-password-secret-storage.md)); RBAC blocks workload service accounts. The data plane is the only path to the credential, and it gates on SA-token validation + HarborAccess match.
- **24h rotation bounds blast radius.** A leaked robot password is valid until the next reconciler rotation, not indefinitely ([ADR-0003](0003-persistent-robots-per-harboraccess.md)).
- **Per-CR scope.** Each HarborAccess gets its own robot with explicit project-level permissions. A leaked robot can only access its specific projects, not the Harbor instance broadly.

This is a meaningful improvement over the baseline (`imagePullSecrets` in workload namespaces, readable by any process in the pod, no automatic rotation). It is not the JWT-shaped improvement ADR-0005 aimed for, because that aim was unreachable.

### Operational

- The bridge's data plane is significantly simpler. No JWT minting, no cache, no 401-retry. Handler shrinks ~120 LOC; ~700 LOC of token-client and cache code is removed.
- One fewer external dependency on Harbor's `/service/token` endpoint from the bridge — containerd carries that load directly, as it always does for any registry.
- The CR's `spec.tokenTTL` now bounds **how long kubelet caches the credential**, not the JWT's wall-clock expiry. If a 24h-bounded rotation happens within that TTL, cached credentials become stale and pulls fail until kubelet expires the cache (typically <1 min after `spec.tokenTTL`). Operators tune `tokenTTL` and rotation interval to their tolerance.

### Architectural

- Bridge stays clearly two-component (control plane reconciler + data plane HTTP server) per [ADR-0002](0002-bridge-control-plane-data-plane-split.md). The data plane is just smaller.
- The post-upstream migration in [ADR-0002](0002-bridge-control-plane-data-plane-split.md) (data plane deletion when goharbor/harbor#17520 lands) still applies. Less code to delete.

## Alternatives considered

- **Keep ADR-0005 as-written, hope containerd accommodates.** Rejected. Above analysis is grounded in containerd's documented auth flow and KEP-4412's response shape; no plausible accommodation exists.
- **Build a custom registry-token proxy in the bridge that containerd's `WWW-Authenticate` realm points to.** Rejected. Requires overriding Harbor's registry-config such that the realm points at the bridge instead of Harbor itself. Cluster-by-cluster Harbor configuration, deeply intrusive, fragile across Harbor upgrades. Not justified by the marginal security gain over rotation-bounded basic auth.
- **Issue ephemeral robots per request.** Rejected for the same reasons [ADR-0003](0003-persistent-robots-per-harboraccess.md) rejected ephemeral robots: Harbor admin-API throughput, audit-log noise, failure mode at pod start. Persistent robots with bounded rotation remain the right shape.

## Migration

For the project's own history (not for external operators — there are no external users of pre-pivot bridge yet):

- ADR-0005 status updated to `Superseded by ADR-0013`.
- ADR-0007 status updated to `Superseded by ADR-0013`. Its design principles (generation-keyed lazy invalidation, lazy over active) remain applicable to any future cache the bridge introduces.
- `bridge/dataplane/harbor_token.go` and `bridge/dataplane/cache.go` removed.
- `github.com/jellydator/ttlcache/v3` dependency removed.
- `github.com/golang-jwt/jwt/v5` dependency retained (used by `oidc_test.go` for SA-token signing).
