# 5. Docker token issuance via Harbor /service/token

## Status

Accepted

## Context

When the kubelet credential-provider plugin asks the bridge for credentials, the bridge must return something a docker/containerd client can use. Two viable returns:

1. **Robot credentials.** Username and password of the persistent Harbor robot. Plugin returns them as Basic Auth to kubelet; kubelet hands them to containerd; containerd uses them on every layer fetch.
2. **A Docker registry v2 bearer JWT.** The bridge calls Harbor's `/service/token` endpoint (the standard Docker registry v2 token server) authenticating with the robot credentials, receives a short-lived JWT scoped to specific repositories, and returns that JWT.

## Decision

The bridge returns a Docker registry v2 bearer JWT obtained from Harbor's `/service/token` endpoint.

- Bridge holds robot credentials in memory only (loaded from a Kubernetes Secret, never logged).
- For each authorised plugin request, the Data Plane calls `GET /service/token` against Harbor with HTTP Basic Auth using the matched robot's credentials. Scope is built from `HarborAccess.spec.permissions`.
- The JWT (not the robot password) is returned to the plugin in the `CredentialProviderResponse` as the password, with the username field set to the docker convention for bearer auth.

## Consequences

- The robot password never leaves the bridge process. A compromised kubelet, plugin, or node only ever sees short-lived JWTs.
- Token TTL is bounded by Harbor's token-server configuration and by our `spec.tokenTTL`. Default 1h, min 5m, max 24h per CRD validation.
- The bridge depends on Harbor's `/service/token` endpoint on every cache miss. We mitigate by caching minted JWTs in `ttlcache` (see [ADR-0007](0007-cache-invalidation-on-cr-change.md)).
- We do not implement JWT minting. Harbor is the authoritative token server; we are a client.

## Alternatives considered

- **Return robot credentials.** Rejected: a long-lived secret distributed to every node is a known anti-pattern. A leak via container escape, kubelet log, or compromised plugin binary would expose project-wide push/pull for up to 24h (the rotation interval). Bearer JWTs are scoped to specific repositories and expire in minutes to hours.
- **Mint our own JWTs and let Harbor accept them.** Rejected: requires Harbor to trust our signer, which is exactly the upstream feature we are bridging for. We would be reinventing the gap.
