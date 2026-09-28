# 33. Sonatype Nexus Repository as a second registry backend

## Status

Proposed (2026-09-28). The maintainer approves or changes it. Every
decision below is marked **maintainer may change before implementation**;
none of it is implemented beyond the Nexus REST client
(`bridge/controlplane/nexus`), which nothing wires in yet.

Extends ADR-0002 (control plane / data plane), ADR-0010 (identity),
ADR-0011 (Secret storage), ADR-0012 (ownership markers), ADR-0018 and
ADR-0031 (naming), ADR-0023 (lifecycle, rotation promise), ADR-0025
(data plane serving) and ADR-0026 (one audience, selected objects).

## Context

Operators who run Sonatype Nexus Repository as their container registry
want the same thing Harbor users get from this project: a workload pulls
with the ServiceAccount it runs as, without `imagePullSecrets`. Nexus has
no robot accounts and no OIDC trust policies. What it has, and what the
bridge can manage over its REST API (`/service/rest`), is local users,
roles, and the built-in repository-view privileges Nexus creates with
every repository (`nx-repository-view-<format>-<repo>-<action>`).

Sources: the nexus-public source at `release-3.96.3` (classes named below),
the community OpenAPI spec of the REST API, Sonatype's documentation, and a
local `sonatype/nexus3:3.76.1` container (the last release before the
Community Edition's EULA gate; its `/v1/system/eula` answers 404). The
runtime checks are listed under "Verified at runtime"; everything else is
from source or documentation and marked so.

The differences to Harbor that shape the design:

1. **The docker bearer token outlives the password.** Nexus's `DockerToken`
   realm hands out one persistent API key per user as the docker bearer
   token. Every `/v2/token` call returns the same key. A password change
   does not revoke it. Neither does disabling the user (the token is
   refused while the user is disabled and valid again after re-enabling),
   and neither does deleting and re-creating the user under the same id:
   the re-created user even gets the same token back. The key is dropped
   only when it is presented while its user id does not exist
   (`BearerTokenRealm.doGetAuthenticationInfo` deletes it on
   `UserNotFoundException`), or by the orphaned-API-key purge task
   (`PurgeApiKeysTask`). Everything but the purge task was observed on
   Nexus 3.76.1. In
   the 3.96.3 source a status change away from `active` and a user
   deletion post `UserPrincipalsExpired`, whose handler in
   `ApiKeyServiceImpl` deletes the user's keys; on 3.76.1 neither had that
   effect. Why is not known (one hypothesis: the event names the source
   `default` while the key's principal carries the realm name
   `NexusAuthenticatingRealm`). A leaked Harbor registry token is a JWT
   that expires on its own (30 minutes by default) and a leaked robot
   password dies at the next rotation. For a leaked Nexus bearer token no
   expiry was found (not in the source read; at runtime it outlived every
   test, minutes only), so nothing short of retiring its user id ends
   it.
2. **No user lookup by id.** `GET /v1/security/users?userId=<term>` matches
   every user whose id *begins* with the term, case-insensitively
   (`AbstractUserManager.matchesCriteria`; verified), without pagination.
   The client must match exactly on its side.
3. **No robot identity to detect re-creation.** `ApiUser` has no numeric
   id or revision, so Harbor's `robot-id` annotation (ADR-0023) has no
   equivalent.
4. **"Already exists" has no status of its own.** A duplicate role is 400
   (`RoleApiResource.ROLE_UNIQUE`), a duplicate user is 500
   (`DuplicateUserException`, text/plain; verified).
5. **Nexus drops unknown roles on user create** instead of refusing
   them (verified), and strips a repository's privileges from every role
   when the repository is deleted, without restoring them when it is
   re-created (verified).
6. **Failed-login rate limiter.** Since 3.93 Nexus answers credentials that
   failed three times with 429 and `Retry-After` (30 s, doubling up to
   900 s), for Basic and token auth, in memory per node (documentation and
   source; not verifiable on 3.76.1). Whether it keys on the user id or on
   the client IP is not known.
7. **Permissions.** Creating users and assigning roles needs
   `nexus:users:*` and `nexus:roles:*`; changing a password needs
   `nexus:*`, i.e. `nx-all` (`UserApiResource.changePassword`). Any
   identity that can create users and assign roles can give itself
   `nx-admin`: the bridge's Nexus credential is admin-equivalent either
   way.
8. **The data plane ignores the image today** (`bridge/dataplane/handler.go`,
   `Request.Image` is audit-only). With two registries it must route.
9. **Secret names.** The robot Secret is `robot-<haNs>.<haName>`
   (`bridge/internal/robotsecret`). A Nexus Secret named
   `robot-nexus.<ns>.<name>` would equal the Secret of a HarborAccess in
   namespace `nexus` named `<ns>.<name>` (custom resource names may carry
   dots).
10. **Formats.** Since 3.94 Nexus serves OCI repositories in a format of
    its own (`nx-repository-view-oci-…`); 3.76.1 has no `oci` format
    (`POST /v1/repositories/oci/hosted` is 404).
11. **Licensing.** Community Edition 3.77 and later require accepting a
    EULA (`/v1/system/eula`) before use. Accepting it is a legal act of
    the operator. This project never accepts it on anyone's behalf.

## Decision

### a. A `NexusAccess` CRD (maintainer may change before implementation)

New kind `NexusAccess`, group `nexus.aetherize.io`, version `v1alpha1`,
namespaced, with HarborAccess's identity model (ADR-0010):

```yaml
apiVersion: nexus.aetherize.io/v1alpha1
kind: NexusAccess
metadata: {name: web, namespace: team-a}
spec:
  serviceAccountRef: {namespace: team-a, name: web}
  trustPolicy: {issuer: https://kubernetes.default.svc, audience: <BRIDGE_AUDIENCE>}
  tokenTTL: 1h
  repositories:
    - {name: docker-hosted, access: pull, format: docker}
    - {name: ci-images, access: "pull,push"}
```

- `serviceAccountRef` as in HarborAccess, except that `namespace` has
  `MaxLength=63` (a namespace is an RFC 1123 label). HarborAccess allows
  253; for Nexus the naming proof below needs the real limit, and
  `IdentityName` refuses longer values.
- `trustPolicy {issuer, audience}` as in HarborAccess; the audience must
  be the bridge's (ADR-0026, `AudienceMismatch`).
- `tokenTTL`: the same `Duration` type and the same CEL rule as
  HarborAccess (Go duration, 5m to 24h, unchanged legacy values ratchet).
- `repositories[]` (`MinItems=1`, `listType=map`, `listMapKey=name`):
  `name` with Nexus's repository pattern `^[a-zA-Z0-9-][a-zA-Z0-9_.-]*$`
  and `MaxLength=167` (so every derived privilege name stays within 200
  characters), which rejects `*`; `access` one of `pull`, `push`,
  `pull,push`; `format` one of `docker`, `oci`, default `docker`.
- `metadata.name` at most 63 characters (CEL on the root, as HarborAccess:
  it becomes a label value).
- Status mirrors HarborAccess: `user {userId, roleId, passwordSecretRef,
  lastRotated}`, `observedGeneration` advanced only when the generation is
  fully applied (ADR-0023), conditions `Ready`, `UserProvisioned`,
  `TrustPolicyApplied`. Reasons: `AudienceMismatch`, `IssuerMismatch`,
  `InvalidSpec`, `UserConflict`, `RoleConflict`, `UserDisabled`,
  `RepositoryNotFound`, `PasswordRejected` (the operator's password
  validator refused the generated password), `NexusError`,
  `NexusAuthFailed`, `NexusRateLimited`, `DeletionBlocked`.
- Finalizer `nexus.aetherize.io/user`, and with a selector
  `nexus.aetherize.io/user-<instance>` (ADR-0026;
  `BRIDGE_NEXUSACCESS_SELECTOR`).

Why a separate kind: Nexus has repositories and formats, Harbor projects;
one kind with a `backend` discriminator would make half of every spec
invalid for the other backend, and RBAC could not grant one registry
without the other. Alternatives: `spec.backend` on HarborAccess
(rejected, above); group `harbor.aetherize.io` (one group for RBAC and
the chart, but a Harbor name for a Nexus object); a neutral
`registry.aetherize.io` for future backends (then HarborAccess is the odd
one out; renaming it is a breaking change).

### b. One local user and one role per identity (maintainer may change before implementation)

- **Role** `bridge-<cluster>.<saNs>.<saName>` (the identity name) holds the
  repository-view privileges. It persists for the identity's lifetime.
- **User** `<identity name>_<generation>` with a 64-bit random generation
  (16 hex digits) holds exactly that role. Every rotation replaces the
  user under a new generation (decision c).
- Naming (`nexus.IdentityName`, `RoleID`, `UserID`, `ParseUserID`,
  `OwnsName`): the ADR-0018 argument, over inputs that `IdentityName`
  itself checks to be DNS labels (cluster and namespace at most 63
  characters, ServiceAccount name at most 253). User ids are capped at
  190 characters (Nexus's id columns are VARCHAR(200); a 201-character
  id fails with 500, verified), identity names at 173. Past the cap the
  name keeps `bridge-<cluster>.`, cuts `<ns>.<sa>` and appends `.` plus 16
  hex digits of the SHA-256 of the natural name. With cluster and
  namespace of at most 63 characters at least 85 characters remain, so
  the cut always falls inside the ServiceAccount name and the ADR-0031
  disjointness holds unconditionally (three dots versus two). The
  generation separator `_` occurs in no DNS label, so a user id splits
  uniquely at its last `_`. Everything is lower-case: Nexus's search is
  case-insensitive, the ownership check is not. Pinned by
  `FuzzNexusName_Injective` and `naming_test.go`. Nexus accepts `--`, so
  unlike Harbor (ADR-0031) such identities get names.
- Ownership markers, the ADR-0012 contract on Nexus objects: user
  `firstName` = `managed-by=harbor-workload-identity-bridge cluster=<c>`,
  `lastName` = `nexusaccess=<ns>/<name>`, `emailAddress` =
  `nexus-bridge@harbor-workload-identity-bridge.invalid` (required by
  Nexus; duplicates are allowed, verified); role `description` = the full
  token list. Ownership of a user or role requires the name prefix
  (`OwnsName`), the cluster tag and the owning NexusAccess, as in
  ADR-0023's `robotOwnedBy`.
- Privileges per access, least privilege verified with `crane` against
  3.76.1: `pull` = `read` (pull, tag list and catalog work with it alone);
  `push` = `add`, `edit` (without `edit` even a new image fails, without
  `read` a push fails); `pull,push` = `add`, `edit`, `read`. `browse` and
  `delete` are never granted. As with Harbor, a pusher declares
  `pull,push` (`nexus.RepositoryPrivileges`).
- Order: create role, then user (Nexus would drop a missing role
  silently); revoke user, then role. The janitor sweeps orphan roles.

Alternatives: one user per identity without a role, holding the
privileges directly (Nexus users hold roles, not privileges); one shared
role per NexusAccess spec (roles would have to be reference-counted
across identities); a hash-only name (probabilistically injective and
unreadable, rejected in ADR-0018).

### c. Rotation replaces the user under a new id (maintainer may change before implementation)

The brief for this ADR proposed rotating by deleting and re-creating the
user with a new password (S3 below), on the premise that this revokes the
docker bearer token. The runtime check disproved it (Context 1). The
decision is therefore:

- A rotation creates the identity's user under a fresh generation with a
  new password (`GeneratePassword`: 48 characters from `crypto/rand`,
  every class present, no `&` or `[` (NEXUS-40119), no `:`, quotes, `\`,
  `%`, `/` or whitespace), writes the Secret, and only then deletes the
  previous user. The old user id never exists again, so its token is
  refused (verified: 401 after the old user is deleted, while the new
  user's token works).
- There is no instant without a valid user, unlike S3 and unlike Harbor's
  `RefreshSec`, where the old password dies before the Secret is written.
- The scheduled rotation keeps ADR-0023's promise: the Secret carries
  `rotation-not-before`, the data plane caps kubelet's cache duration at
  it, and the reconciler rotates only after it plus the safety margin.
- A user an administrator disabled or locked is not rotated (a new
  generation would undo the administrator's decision) and is reported as
  `UserDisabled`; the bridge never re-enables it.
- Before creating a generation the reconciler records it in the Secret
  (a pending-generation annotation), so the janitor can tell a user
  being created from an orphan (`ApiUser` has no creation time).
- Forced rotation (the Secret is missing, incomplete, or names a user
  that does not exist) follows the same path.

Alternatives (runtime results on 3.76.1):

- **S1, change the password** (`PUT …/change-password`, text/plain;
  `application/json` is 415): the old password is refused, the bearer
  token stays valid and the next `/v2/token` returns the same token.
  Needs `nx-all`. Rejected: rotation would not bound a leaked token.
- **S2, disable, change, enable**: the token is refused while disabled
  and valid again after re-enabling. Rejected for the same reason; a
  crash also leaves the user disabled.
- **S3, delete and re-create under the same id** (the brief's proposal):
  the old token stays valid (tested without presenting it in between,
  with 0 s, 10 s and 30 s between delete and create). Rejected. It
  revokes only if something presents the token while the user is
  absent.
- **S3 plus the purge task**: delete, run the orphaned-API-key purge task
  (`PurgeApiKeysTask`), re-create. Needs a task of that type to exist
  (whether the bridge could create one over the REST API was not
  checked), runs asynchronously, and leaves the identity without a user
  meanwhile. Not tested at runtime.
- **S3 plus presenting the token**: the bridge fetches the user's token
  (it is the same for every caller) and presents it between delete and
  create. Needs registry access from the bridge and depends on a Nexus
  internal. Rejected.
- **S4, user tokens**: Nexus Pro only.

### d. A missing repository fails closed (maintainer may change before implementation)

Before writing the role the reconciler checks each privilege with
`GET /v1/security/privileges/{name}` (404 for a repository that does not
exist in that format, verified; names are case-sensitive, verified). If
any is missing: `Ready=False, reason=RepositoryNotFound` naming the
repositories, no role write (Nexus would refuse it with 400, verified),
and the data plane issues no credentials for that NexusAccess. The user
and the role stay, so recovery needs no rotation; when the repository
exists again the next pass restores the privilege Nexus stripped.

Why: a partial grant makes a typo or a deleted repository look Ready and
silently changes what a workload may pull. Alternative: **partial grant**
(grant the repositories that exist, report the rest in a separate
condition, stay Ready). Kinder to availability: a deleted repository does
not stop pulls from the others.

### e. Robot Secret contract per kind (maintainer may change before implementation)

- Name `nexususer-<ns>.<name>`: a constant prefix per kind, neither of
  which is a prefix of the other, so Harbor and Nexus Secret names are
  disjoint by construction (Context 9), with robotsecret's hash
  truncation. `FuzzName_Injective` gets a cross-kind case.
- Labels: the existing `harbor.aetherize.io/managed-by` and `…/cluster`,
  plus `harbor.aetherize.io/access-kind: NexusAccess` and
  `…/nexusaccess-namespace`, `…/nexusaccess-name`. No
  `harboraccess-*` labels: the Harbor janitor skips managed Secrets
  without them (`robotsecret.Owner` is not ok,
  `bridge/controlplane/janitor.go` `sweepSecrets`), and so does the
  Harbor reconciler's Secret watch.
- Keys `username` (the user id) and `password`; annotations
  `rotation-not-before` (unchanged meaning) and the pending generation.
  The contract lives in `bridge/internal/robotsecret` next to Harbor's.

### f. The data plane routes by registry host (maintainer may change before implementation)

- Each backend has configured registry hosts (`host[:port][/path
  prefix]`). The handler parses the requested image's reference and
  matches it segment-aware: the host and port exactly, the path prefix
  on `/` boundaries (`host/nexus` matches `host/nexus/img`, not
  `host/nexus-old/img`). The longest match wins; identical entries in two
  backends are refused at startup.
- Two backends must not share a `host[:port]`: kubelet caches per registry
  host (`cacheKeyType: Registry`, ADR-0016), so a shared host would serve
  one backend's credentials for the other's images.
- Only then does it look for that kind's objects that name the token's
  ServiceAccount and the bridge's audience. An image that matches no
  backend is refused (`no_backend`) when more than one backend is
  configured.
- With only Harbor configured the handler behaves exactly as today: no
  registry hosts needed, the image stays audit-only.
- The plugin does not change. It already sends the image, and its wire
  types stay as they are (ADR-0015).

### g. A backend seam in the control plane (maintainer may change before implementation)

- An interface both backends implement, roughly: `Kind`,
  `AccountName(identity)`, `Get`, `ListOwned(cluster)`,
  `MissingGrants(grants)`, `Create(name, owner, grants) (account,
  password)`, `Converge(account, owner, grants)`, `Rotate(account)
  (account, password)`, `Retire(old account)` (Harbor: no-op; Nexus:
  delete the previous generation after the Secret write), `Delete`.
- Shared: identity naming rules, rotation scheduling, conditions,
  ownership checks, the janitor skeleton. One controller per CRD kind.
  A conformance test suite runs the same lifecycle cases against the
  Harbor and the Nexus fakes.
- Nothing in `bridge/controlplane` imports `bridge/dataplane` (ADR-0002);
  the shared Secret contract stays in `bridge/internal/robotsecret`.

### h. Chart (maintainer may change before implementation)

- `nexus.enabled`, `nexus.url`, `nexus.adminCredsSecret`, `nexus.caSecret`,
  `nexus.allowInsecureHTTP`, `nexus.registryHosts`, and
  `harbor.registryHosts` (required once Nexus is enabled).
- Env `BRIDGE_NEXUS_URL`, `BRIDGE_NEXUS_ADMIN_DIR`, `BRIDGE_NEXUS_CA_FILE`,
  `BRIDGE_NEXUS_ALLOW_INSECURE_HTTP`, `BRIDGE_NEXUS_REGISTRY_HOSTS`,
  `BRIDGE_HARBOR_REGISTRY_HOSTS`, `BRIDGE_NEXUSACCESS_SELECTOR`. The
  admin credentials are re-read per request (the Harbor client's
  `WithCredentialSource`, #136); the Nexus client offers the same option.
- RBAC for `nexusaccesses`, `nexusaccesses/status`,
  `nexusaccesses/finalizers`, and the CRD under `crds/`.
- Template validation: every registry host is covered by
  `plugin.matchImages` under kubelet's rules (globs in the domain only,
  a literal path prefix) when the plugin is chart-managed, and no two
  backends share a `host[:port]`.

### i. e2e (maintainer may change before implementation)

- `sonatype/nexus3` pinned by digest to a release before 3.77 (OSS, no
  EULA; 3.76.1 was used for this ADR; its image is linux/amd64 only, so
  it runs emulated on arm64 hosts, about 85 s to writable), in the kind
  harness as its own tofu test file (`tests/03-nexus.tftest.hcl`,
  `make e2e-nexus`), with modules for Nexus, its seed (realms, anonymous
  off, hosted docker repositories) and a NexusAccess scenario.
- Assertions include the ones this ADR rests on: the old bearer token is
  refused after a rotation, a missing repository is `RepositoryNotFound`,
  `DeletionBlocked` while Nexus is down, routing with both backends.
- Community Edition 3.77 and later need the operator to accept the EULA
  themselves; the docs say so, the harness never does it. The e2e
  therefore cannot cover 3.77+ (the rate limiter of 3.93+, the `oci`
  format of 3.94+, any change to the token behaviour); see question 4.
- `TestLive_AgainstNexus` (`bridge/controlplane/nexus/client_live_test.go`)
  runs the client against a disposable Nexus when `NEXUS_LIVE_URL` is set
  and pins the behaviour above, including the bearer-token checks.

### j. Security (maintainer may change before implementation)

- The bridge's Nexus credential is admin-equivalent (Context 7). The
  client never needs `nx-all`: it creates, updates and deletes users and
  roles and reads privileges (`nexus:users:*`, `nexus:roles:*`,
  `nexus:privileges:read`); a dedicated role with just these is still
  admin-equivalent and says so in the docs. The client deliberately has
  no change-password call: decision c does not need it, and it would
  need `nx-all`.
- The client follows the Harbor client's hardening: 30 s per call
  (configurable), header and TLS timeouts, TLS 1.2 floor, optional
  private CA, https unless explicitly allowed, no `user:password@` in the
  URL, no redirects (they would re-send the admin credentials and a new
  user's password), bounded bodies (16 MiB decoded, 4 KiB of an error),
  ids restricted to `[A-Za-z0-9._-]` before they enter a path, no
  logging, and an error message from Nexus that contains a credential or
  password of the request is withheld as a whole
  (`FuzzRenderMessage_WithholdsSecrets`). The status endpoints get no
  credentials (they need none, verified). A 404 never reads as "gone"
  unless Nexus's own listing confirms it: `DeleteUser` and `DeleteRole`
  re-list after a 404, and a 404 from a listing endpoint (a URL that does
  not point at Nexus) is `ErrUnexpectedStatus`, not `ErrNotFound`, so a
  misconfigured URL cannot release a finalizer while the user lives on.
- 429: the client never retries; it returns `ErrRateLimited` with the
  parsed `Retry-After` (seconds or HTTP date). The reconciler waits at
  least that long (`NexusRateLimited`), capped by its resync interval,
  never hot.
- Threat model additions, open until the maintainer rates them:
  theft of the Nexus admin credential (B4 for Nexus, admin-equivalent);
  a leaked docker bearer token stays valid until its user is retired
  (bounded by the rotation interval only because of decision c); a
  NexusAccess author grants any repository (T8's twin); Nexus's
  `nx-anonymous` role or the DefaultRole realm widens every bridge
  user's rights beyond the spec, which the bridge cannot see from the
  user; the failed-login rate limiter lets stale credentials on one node
  lock out that node's pulls if it keys on the client IP; Nexus's
  in-memory limiter state is per node, so HA Nexus multiplies the
  allowance.

### k. Open questions for the maintainer

1. **Group**: `nexus.aetherize.io`, `harbor.aetherize.io` or
   `registry.aetherize.io` (decision a).
2. **Rotation**: the new-id rotation (decision c) versus S3 plus the purge
   task, or accepting S1/S3 with the token-survival risk documented. The
   new-id rotation makes the Nexus user id change daily, which operators
   see in Nexus's audit log.
3. **Missing repository**: fail closed or partial grant (decision d).
4. **EULA**: whether a maintainer accepts the Community Edition EULA for a
   manual verification run of 3.77+ (the rate limiter's key, the `oci`
   format, whether 3.96 revokes tokens on delete). A legal decision this
   ADR does not make.
5. **Credential self-check**: should the bridge verify the stored password
   by authenticating as the user? An administrator's password change is
   otherwise invisible until pulls fail, but every failed check counts
   against the rate limiter.
6. **Community Edition limits**: CE caps `/repository/*` requests at
   100,000 a day and components at 40,000 (documentation). Whether docker
   connector traffic counts is not verified; the docs should warn.
7. **Harbor optional**: whether a bridge may run with Nexus only
   (`harbor.url` is required today).

## Verified at runtime

Nexus `sonatype/nexus3:3.76.1` in a local container bound to 127.0.0.1,
2026-09-28, `curl` and `crane` against the REST API and a hosted docker
repository with an HTTP connector, `DockerToken` realm active, anonymous
access off; removed afterwards.

| Behaviour | Result |
| --- | --- |
| Duplicate user id on create | 500, text/plain `DuplicateUserException: User … already exists.` |
| Duplicate role id on create | 400, `Role '…' already exists, use a unique roleId.` |
| Role update with body id ≠ path id | 409 |
| Role create/update with a privilege that does not exist | 400 |
| User update with an unknown role | 400, validation list `[{"id":"roles","message":…}]` |
| User update with body id ≠ path id | 400 |
| User create with an unknown role | 200, the role is dropped |
| User create without roles / without e-mail | 400 each |
| Two users with the same e-mail | allowed |
| User id of 200 / 201 characters | 200 / 500 |
| `--`, `_` and a trailing `.json`/`.xml` in user ids | accepted; PUT and DELETE address the right user |
| `GET /v1/security/users?userId=bridge-rt.` | also returns `BRIDGE-RT.ns.upper`: case-insensitive prefix |
| `DELETE …/users/{id}?realm=NexusAuthenticatingRealm` of a missing user | 404 |
| Deleting a role | removes it from every user |
| Deleting a repository | strips its privileges from every role; re-creating it does not restore them |
| `GET /v1/security/privileges/{name}` | 200 for an existing repository, 404 otherwise; names case-sensitive |
| `change-password` | 204 with text/plain, 415 with application/json |
| Request without credentials / wrong password | 403 / 401 |
| `/v1/status`, `/v1/status/writable` without credentials | 200 |
| Bearer token after password change | still valid; `/v2/token` returns the same token |
| Bearer token while disabled or locked / after re-enabling | 401 / valid again |
| Bearer token after delete and re-create under the same id | still valid (0 s, 10 s and 30 s gap); the new user gets the same token |
| Bearer token presented while its user is absent | 401, and stays 401 after re-creation |
| Bearer token after replacing the user under a new id | old token 401, new token 200 |
| Least privilege (crane) | pull, tag list, catalog: `read`; push: `add`+`edit`+`read`; `add`+`browse`+`read` cannot push |
| Generated passwords (`GeneratePassword`) | authenticate for the REST API and `/v2/token` |
| `oci` format / EULA endpoint | absent (404) on 3.76.1 |

`TestLive_AgainstNexus` repeats the client-relevant rows and the
bearer-token rows; it passed against this container.

## Consequences

- A second registry is supported with the same identity model, the same
  rotation promise and the same revocation guarantees as Harbor, at the
  price of a second CRD, a routing step in the data plane and a
  backend seam in the control plane.
- Nexus user ids change at every rotation; the role id, the Secret name
  and the NexusAccess stay. Each identity has one user, two for the
  moment of a rotation.
- A docker bearer token leaked from a node stays valid until the next
  rotation or deletion retires its user (at most `PasswordRotationInterval`
  plus the margin), not until its own expiry, which Nexus does not have.
- Orphan API keys of retired users stay in Nexus's store until presented
  (then deleted) or purged; they grant nothing, because their user ids
  never exist again.
- The bridge holds an admin-equivalent Nexus credential; SECURITY.md and
  the threat model must say so.
- The e2e covers only pre-3.77 releases unless an operator accepts the
  EULA; behaviour specific to later releases stays unverified.
- A Harbor-only installation sees no change.
