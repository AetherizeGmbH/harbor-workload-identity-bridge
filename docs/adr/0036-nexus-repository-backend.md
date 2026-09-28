# 36. Sonatype Nexus Repository as a second registry backend

## Status

Proposed (2026-09-28). The maintainer approves or changes it. Every
decision below is marked **maintainer may change before implementation**.
Implemented so far: the Nexus REST client (`bridge/controlplane/nexus`),
the `NexusAccess` API, the Secret contract, the configuration, the
control plane (reconciler, janitor, rate-limit backoff), the data plane's
routing by registry host, the wiring of both into the bridge's entry
point when `BRIDGE_NEXUS_URL` is set, the chart's `nexus.*` values, and
the e2e harness (`make e2e-nexus`, not yet run). Where the
implementation departs from the decisions below, "Implementation notes"
says so.

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

Sources: the nexus-public source at `release-3.96.3` (classes named below)
and, where behaviour changed between releases, at the tags named with it;
the community OpenAPI spec of the REST API; Sonatype's documentation; and a
local `sonatype/nexus3:3.76.1` container (the last release before the
Community Edition's EULA gate; its `/v1/system/eula` answers 404). The
nexus-public tree of 3.96.3 holds no implementation of the docker or OCI
formats, so what those formats do on 3.77 and later cannot be read from
source. The
runtime checks are listed under "Verified at runtime"; everything else is
from source or documentation and marked so.

The differences to Harbor that shape the design:

1. **The docker bearer token outlives the password.** Nexus's `DockerToken`
   realm hands out one persistent API key per user as the docker bearer
   token. Every `/v2/token` call returns the same key. A password change
   does not revoke it. Neither does disabling the user (the token is
   refused while the user is disabled and valid again after re-enabling),
   and neither does deleting and re-creating the user under the same id:
   the re-created user even gets the same token back. The token realm
   accepts a user whose status counts as active, which is `active` and
   `changepassword` (`UserStatus.isActive`, `BearerTokenRealm`; for
   `changepassword` verified). The key is dropped only when it is
   presented while its user id does not exist
   (`BearerTokenRealm.doGetAuthenticationInfo` deletes it on
   `UserNotFoundException`), or by the orphaned-API-key purge task
   (`PurgeApiKeysTask`). Everything but the purge task was observed on
   Nexus 3.76.1. In the 3.96.3 source a status change of a user whose
   status counts as active to any other status, and a user deletion, post
   `UserPrincipalsExpired`, whose handler in `ApiKeyServiceImpl` deletes
   the user's keys; on 3.76.1 neither had that effect. Why is not known
   (one hypothesis: the event names the source `default` while the key's
   principal carries the realm name `NexusAuthenticatingRealm`). A leaked
   Harbor registry token is a JWT that expires on its own (30 minutes by
   default) and a leaked robot password dies at the next rotation. For a
   leaked Nexus bearer token no expiry was found (not in the source read;
   at runtime it outlived every test, minutes only), so nothing short of
   retiring its user id ends it.
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
5. **Writes that succeed without doing what they say.**
   - A user create naming a role that does not exist is not refused
     (verified). The answer echoes the request, unknown role included
     (`UserApiResource.createUser` returns the object it passed to
     `DefaultSecuritySystem.addUser`). Nexus stores the role id with the
     user (`UserManagerImpl` stores the ids unfiltered), hides it from
     every read, and the user gains the role as soon as a role with that
     id is created (verified). A user update naming an unknown role is
     refused with 400 (verified).
   - A role create naming a privilege that does not exist is refused with
     400 (verified; `validateContainedRolesAndPrivileges` at
     `release-3.90.5`, `release-3.91.0` and `release-3.96.3`). A role
     update is refused the same way up to 3.90.x (verified on 3.76.1;
     source up to `release-3.90.5`). From 3.91.0 on Nexus
     removes the missing privileges, stores the rest and answers 204
     (`SecurityConfigurationManagerImpl.validateAndCleanOrphanedPrivileges`,
     source at `release-3.91.0` to `release-3.96.3`).
   - Deleting a repository strips its privileges from every role without
     restoring them when it is re-created, and deleting a role removes it
     from every user without restoring it when the role is re-created
     (both verified).
6. **Failed-login rate limiter** (source; not verifiable on 3.76.1).
   Since 3.93.0 Nexus counts failed logins in memory on each node
   (`AuthRateLimiterServiceImpl`). The key is the username of Basic Auth,
   or the SHA-256 of an API-key token; the client IP is only written to
   the audit event (`NexusBasicHttpAuthenticationFilter`). The failure
   that takes the count past `nexus.auth.ratelimit.max-attempts`
   (default 3) is answered with 429 and `Retry-After` (30 s, doubling per
   further failure up to `nexus.auth.ratelimit.max-delay-seconds`,
   default 900 s). On 3.93 a correct password still succeeds and clears
   the count. From 3.94.0 on a username past the limit gets 429 before
   Nexus checks the password, so the correct password is refused too, and
   each such request refreshes the entry's idle timer (Guava
   `expireAfterAccess`). The count has no timestamp, so `Retry-After` is
   not when requests succeed again: the block ends only after
   max-delay-seconds without any request for that username, when an
   administrator updates the user or changes its password
   (`AuthRateLimiterResetListener`), or when Nexus restarts. Whether the
   docker connector's logins pass through the same filter is not known
   (the docker format's code is not in nexus-public 3.96.3).
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
    its own (Sonatype's release notes, not verified); 3.76.1 has no `oci`
    format (`POST /v1/repositories/oci/hosted` is 404, verified).
    According to the community OpenAPI spec (not
    verified) OCI clients authenticate through a separate "OCI Bearer
    Token Realm" (`OciAttributes.forceBasicAuth`), OCI repository names
    must be lower-case, and, according to Sonatype's documentation (not
    verified), its privileges are `nx-repository-view-oci-…`. Whether the
    OCI realm's tokens expire, survive a password change or die with
    their user is not known, and decision c rests on exactly that for
    docker.
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
  `pull,push`; `format` an enum with the single value `docker`, the
  default. `oci` is left out until its token realm has been analysed
  (Context 10, question 8); keeping the field makes adding it later a
  non-breaking change.
- `metadata.name` at most 63 characters (CEL on the root, as HarborAccess:
  it becomes a label value).
- Status mirrors HarborAccess: `user {userId, roleId, passwordSecretRef,
  lastRotated}`, `observedGeneration` advanced only when the generation is
  fully applied (ADR-0023), conditions `Ready`, `UserProvisioned`,
  `TrustPolicyApplied`. Reasons: `AudienceMismatch`, `IssuerMismatch`,
  `InvalidSpec`, `UserConflict`, `RoleConflict`, `UserDisabled`,
  `RepositoryNotFound`, `PasswordRejected` (the operator's password
  validator refused the generated password), `NexusError`,
  `NexusAuthFailed`, `NexusRateLimited`, `DeletionBlocked`. Whether
  status should carry `userId` is question 9.
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
  uniquely at its last `_`; `ParseUserID` accepts exactly the ids of the
  shape `UserID` builds (lower-case DNS labels within their limits, a
  natural or truncated identity name, a 16-hex-digit generation), so the
  janitor can use it as a filter. Everything is lower-case: Nexus's
  search is case-insensitive, the ownership check is not. Pinned by
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
- Order: create role, then user; revoke user, then role. A user created
  before its role would hold a hidden reference that takes effect only
  when the role is created (Context 5). `CreateUser` returns the user as
  read back, so the reconciler compares its roles with the requested one
  and treats a missing role as not provisioned. The janitor sweeps orphan
  roles.

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
- Scheduled and forced rotation, and the deletion of previous
  generations, do not depend on the grants: they continue while the
  object is `RepositoryNotFound` (decision d). The leaked-token bound in
  the Consequences needs that.
- A user an administrator disabled or locked is not rotated (a new
  generation would undo the administrator's decision) and is reported as
  `UserDisabled`; the bridge never re-enables it. Its password and token
  are refused meanwhile (verified). A user in status `changepassword`
  counts as active: its password and token keep working (verified), so
  it is rotated like an active user (`UserStatus.Active`), and its
  replacement is created `active`.
- Before creating a generation the reconciler records it in the Secret
  (a pending-generation annotation), so the janitor can tell a user
  being created from an orphan (`ApiUser` has no creation time).
- Forced rotation (the Secret is missing, incomplete, or names a user
  that does not exist) follows the same path.
- A rotation also escapes a rate-limiter block on the workload user
  (Context 6): the new user id is a new key.

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

### d. A missing repository adds nothing and holds back no removal (maintainer may change before implementation)

Every pass the reconciler checks each privilege of the spec with
`GET /v1/security/privileges/{name}` (404 for a repository that does not
exist in that format, verified; names are case-sensitive, verified; the
client confirms a 404 against the built-in `nx-all`). If any is missing:

- It writes the role with exactly the spec's privileges that exist. A
  privilege the spec no longer names is therefore removed in the same
  pass, even when the same edit names a repository that does not exist
  (ADR-0023: a revocation by spec edit is a revocation), and nothing
  missing is added. Nexus would refuse the full list with 400 before
  3.91.0 and store it without the missing entries from 3.91.0 on
  (Context 5).
- `Ready=False, reason=RepositoryNotFound` names the repositories;
  `observedGeneration` is not advanced.
- The data plane issues no credentials for the NexusAccess. The
  reconciler marks the Secret with a `grants-incomplete` annotation
  (decision e) and removes it once every privilege is in the role; the
  data plane refuses while it is present (decision f). The mark lives on
  the Secret, not in the status: only the bridge writes Secrets in its
  namespace, while RBAC may let users write `nexusaccesses/status`.
- Rotation and the deletion of previous generations continue
  (decision c).
- Credentials already issued (kubelet caches up to `rotation-not-before`,
  docker bearer tokens) keep access only to repositories that exist and
  that the spec still names: the role is shared by every credential of
  the identity.
- When the repository exists again, the next pass writes the full list
  (restoring a privilege Nexus stripped) and removes the mark.
- A write that fails because a repository vanished between the check and
  the write is `RepositoryNotFound` as well: before 3.91.0 a 400 that
  stored nothing, after which the reconciler checks and writes again at
  once, so a removal waits for one retry at most; from 3.91.0 on
  `UpdateRole` reads the role back and returns `ErrPrivilegesDropped`
  with the role already stored without the missing privilege.

Why: a partial grant makes a typo or a deleted repository look Ready and
silently changes what a workload may pull, and holding back the whole
role write would hold back removals too. Alternatives: **partial grant**
(the same role write, but stay Ready, keep serving and report the missing
repositories in a separate condition): kinder to availability, a deleted
repository does not stop new credentials for the others. **Freeze** (the
first draft of this decision: no role write while anything is missing):
rejected, because an edit that removed repository A and named a missing
repository C left A granted to every existing password and bearer token.
**Empty role** (write no privileges while anything is missing): the
strictest, but it also cuts cached credentials off the repositories that
exist, which safety does not require once the data plane issues nothing.
A stronger data-plane gate (serve only when the Secret records the
current `metadata.generation` as fully applied) would also close the
seconds between a spec edit and the reconcile, at the price of refusing
every NexusAccess after every edit until the reconciler has run.

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
  `rotation-not-before` (unchanged meaning), the pending generation, and
  `grants-incomplete` (decision d). The contract lives in
  `bridge/internal/robotsecret` next to Harbor's.

### f. The data plane routes by registry host (maintainer may change before implementation)

- Each backend has configured registry hosts (`host[:port][/path
  prefix]`). The handler parses the requested image's reference and
  matches it segment-aware: the host and port exactly, the path prefix
  on `/` boundaries (`host/nexus` matches `host/nexus/img`, not
  `host/nexus-old/img`).
- Two backends must not share a `host[:port]`: kubelet caches per registry
  host (`cacheKeyType: Registry`, ADR-0016), so a shared host would serve
  one backend's credentials for the other's images. Startup refuses such
  a configuration. An image therefore matches the entries of at most one
  backend; a path prefix only narrows which images of its host that
  backend serves, and several entries of one backend may overlap (any
  match selects it).
- Only then does it look for that kind's objects that name the token's
  ServiceAccount and the bridge's audience. An image that matches no
  backend is refused (`no_backend`) when more than one backend is
  configured.
- The check that the Secret holds the object's current identity
  (ADR-0023, today `creds.username` against the one exact name
  `HandlerConfig.RobotUsername` returns) becomes an identity comparison
  for Nexus: the data plane accepts a Nexus Secret only when
  `ParseUserID(username)` returns the NexusAccess's current
  `IdentityName`, whatever the generation. An exact name cannot express
  "this identity, any generation", and dropping the check would hand a
  previous identity's password to the new one.
- It refuses a Nexus Secret that carries `grants-incomplete`
  (decision d).
- With only Harbor configured the handler behaves exactly as today: no
  registry hosts needed, the image stays audit-only.
- The plugin does not change. It already sends the image, and its wire
  types stay as they are (ADR-0015).

### g. A backend seam in the control plane (maintainer may change before implementation)

- An interface both backends implement, roughly: `Kind`,
  `AccountName(identity)` (the stem: Harbor's robot name, Nexus's
  identity name), `Get(identity, current)`, `ListOwned(cluster)`,
  `MissingGrants(grants)`, `Create(identity, owner, grants) (account,
  password)`, `Converge(account, owner, grants)`, `Rotate(identity,
  current) (account, password)`, `Retire(identity, keep)` (Harbor: no-op;
  Nexus: delete every generation of the identity but `keep` after the
  Secret write), `Delete`.
- `current` is the account the Secret names (its `username`, plus the
  pending generation), an input rather than something derived from the
  identity: several Nexus users of one identity can exist (two during a
  rotation, orphans after a crash), and only the Secret says which one
  is current.
- Shared: identity naming rules, rotation scheduling, conditions,
  ownership checks, the janitor skeleton. One controller per CRD kind.
  A conformance test suite runs the same lifecycle cases against the
  Harbor and the Nexus fakes.
- Nothing in `bridge/controlplane` imports `bridge/dataplane` (ADR-0002);
  the shared Secret contract stays in `bridge/internal/robotsecret`.

### h. Chart (maintainer may change before implementation)

- `nexus.enabled`, `nexus.url`, `nexus.adminCredsSecret`, `nexus.caSecret`,
  `nexus.allowInsecureHTTP`, `nexus.registryHosts`,
  `nexus.rateLimitBackoff` (decision j), and `harbor.registryHosts`
  (required once Nexus is enabled).
- Env `BRIDGE_NEXUS_URL`, `BRIDGE_NEXUS_ADMIN_DIR`, `BRIDGE_NEXUS_CA_FILE`,
  `BRIDGE_NEXUS_ALLOW_INSECURE_HTTP`, `BRIDGE_NEXUS_REGISTRY_HOSTS`,
  `BRIDGE_NEXUS_RATE_LIMIT_BACKOFF`, `BRIDGE_HARBOR_REGISTRY_HOSTS`,
  `BRIDGE_NEXUSACCESS_SELECTOR`. The admin credentials are re-read per
  request (the Harbor client's `WithCredentialSource`, #136); the Nexus
  client offers the same option.
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
  refused after a rotation; a missing repository is `RepositoryNotFound`
  and the data plane refuses; an edit that removes repository A and names
  a missing repository C revokes A at once for the existing password and
  bearer token; a `RepositoryNotFound` object still rotates;
  `DeletionBlocked` while Nexus is down; routing with both backends.
- Community Edition 3.77 and later need the operator to accept the EULA
  themselves; the docs say so, the harness never does it. The e2e
  therefore cannot cover 3.77+ (the rate limiter of 3.93+, the role
  update of 3.91+, the `oci` format of 3.94+, any change to the token
  behaviour); see question 4.
- `TestLive_AgainstNexus` (`bridge/controlplane/nexus/client_live_test.go`)
  runs the client against a disposable Nexus when `NEXUS_LIVE_URL` is set
  and checks the rows marked in the last column of the table under
  "Verified at runtime"; the other rows were checked by hand with `curl`
  and `crane`.

### j. Security (maintainer may change before implementation)

- The bridge's Nexus credential is admin-equivalent (Context 7). The
  client never needs `nx-all`: it creates, updates and deletes users and
  roles and reads privileges. A role holding only `nx-users-all`,
  `nx-roles-all` and `nx-privileges-read` suffices (verified:
  `TestLive_AgainstNexus` runs every client call as such a user); it is
  still admin-equivalent and the docs say so. The client deliberately has
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
  (`renderMessage`, `FuzzRenderMessage_WithholdsSecrets`; that the client
  passes the admin password and Basic token in is pinned by
  `TestClient_ErrorsNeverCarryAdminCredentials`). The status endpoints
  get no credentials (they need none, verified).
- Writes are read back where Nexus's answer does not show the result:
  `CreateUser` returns the user as `GetUser` reads it (the create's
  answer echoes the request, Context 5), and `UpdateRole` reads the role
  back and reports privileges Nexus 3.91+ dropped
  (`ErrPrivilegesDropped`).
- A 404 never reads as "gone" unless Nexus confirms it: `DeleteUser`,
  `UpdateUser`, `GetRole`, `UpdateRole` and `DeleteRole` re-list after a
  404, `GetPrivilege` asks the same endpoint for the built-in `nx-all`,
  and a 404 from a listing endpoint or for `nx-all` (a URL that does not
  point at Nexus) is `ErrUnexpectedStatus`, not `ErrNotFound`. A
  misconfigured URL therefore cannot release a finalizer while the user
  or role lives on, nor report `RepositoryNotFound` for a URL problem.
- 429: the client never retries; it returns `ErrRateLimited` with the
  parsed `Retry-After` (seconds or HTTP date). Because `Retry-After` is
  not the recovery time and every request during a 3.94+ block extends
  it (Context 6), a 429 on the admin credential makes the Nexus
  controller stop every call made with that credential for
  `BRIDGE_NEXUS_RATE_LIMIT_BACKOFF` (default 15 minutes, Nexus's default
  max-delay-seconds; an operator who raised
  `nexus.auth.ratelimit.max-delay-seconds` raises it as well), counted
  from the last 429, without probing. Meanwhile every NexusAccess shows
  `NexusRateLimited` and a metric counts the blocked state, so operators
  can alert on it. An administrator's update of the bridge's admin user
  (or a Nexus restart) lifts the block early; the backoff cannot see
  that, and restarting the bridge ends its wait.
- Threat model additions, open until the maintainer rates them:
  - theft of the Nexus admin credential (B4 for Nexus,
    admin-equivalent);
  - a leaked docker bearer token stays valid until its user is retired:
    bounded only because of decision c, and only while the control plane
    can authenticate to Nexus. With note 4's grace the bound is
    `PasswordRotationInterval + RotationSafetyMargin +
    NexusUserRetireGrace` (24 h + 1 min + 5 min) after the user was
    created, plus the reconcile latency;
  - remote lockout of the bridge's admin credential (3.94+): anyone who
    reaches Nexus's HTTP port (every developer with registry access,
    any pod that can reach it) and knows the admin username keeps it
    blocked with four wrong passwords, and any request within
    max-delay-seconds of the previous one keeps it blocked. Rotation,
    the deletion of previous generations and of deleted NexusAccess
    objects' users (revocation, `DeletionBlocked`) then stop, and the
    leaked-token bound above is suspended. Mitigation: a dedicated admin account with
    a long random username (never `admin`), kept only in the admin
    credential Secret; the `NexusRateLimited` alert; limiting who can
    reach Nexus's REST API;
  - the same lockout of a workload identity's user id denies that
    identity's pulls on every node, not only on one (the key is the
    username), if the docker connector's logins pass through the limiter
    (not known, Context 6). The id carries a random 64-bit generation,
    so only readers of the NexusAccess status (`status.user.userId`,
    question 9) or of the Secret can target it, and the next rotation
    escapes the block;
  - a NexusAccess author grants any repository (T8's twin);
  - Nexus's `nx-anonymous` role or the DefaultRole realm widens every
    bridge user's rights beyond the spec, which the bridge cannot see
    from the user;
  - Nexus's in-memory limiter state is per node, so HA Nexus multiplies
    the allowance of wrong passwords.

### k. Open questions for the maintainer

1. **Group**: `nexus.aetherize.io`, `harbor.aetherize.io` or
   `registry.aetherize.io` (decision a).
2. **Rotation**: the new-id rotation (decision c) versus S3 plus the purge
   task, or accepting S1/S3 with the token-survival risk documented. The
   new-id rotation makes the Nexus user id change daily, which operators
   see in Nexus's audit log.
3. **Missing repository**: add nothing and hold back no removal while the
   data plane refuses (decision d), partial grant, or empty role.
4. **EULA**: whether a maintainer accepts the Community Edition EULA for a
   manual verification run of 3.77+ (the rate limiter's behaviour at
   runtime, whether the docker connector's logins count against it, the
   role update of 3.91+, the `oci` format and its token realm, whether
   3.96 revokes tokens on delete). A legal decision this ADR does not
   make.
5. **Credential self-check**: should the bridge verify the stored password
   by authenticating as the user? An administrator's password change is
   otherwise invisible until pulls fail, but every failed check counts
   against the rate limiter, and from 3.94 on the fourth failed check
   blocks the user.
6. **Community Edition limits**: CE caps `/repository/*` requests at
   100,000 a day and components at 40,000 (documentation). Whether docker
   connector traffic counts is not verified; the docs should warn.
7. **Harbor optional**: whether a bridge may run with Nexus only
   (`harbor.url` is required today).
8. **OCI format**: `oci` is added once its token realm has been analysed
   (Context 10): whether its tokens expire, survive a password change or
   a delete and re-create, and die with their user id. It needs 3.94+,
   so question 4 first.
9. **Status user id**: `status.user.userId` maps Nexus's audit log to
   NexusAccess objects, but tells every reader of the object which
   username to lock out (decision j). Keep it, or leave it to the Secret.
10. **Path prefix scope** (added by the implementation review): kubelet
    reuses credentials for every image of a registry host[:port], so a
    path prefix in `nexus.registryHosts` selects requests but does not
    confine credentials (note 21). Answer a request that matched only
    through path-prefixed entries with `cacheKeyType: Image`, at one
    bridge request per image and node, or keep `Registry` and the
    documented limit.

## Implementation notes

The control plane (2026-09-28) implements decisions a to d and j with the
deviations below. The interface contract the parallel workstreams (data
plane, chart, wiring) share fixed some names differently from the
proposal; where the two disagree the contract won, and it is recorded
here.

1. **Secret labels** (decision e). The Secret carries
   `app.kubernetes.io/managed-by: harbor-workload-identity-bridge`, the
   robot Secrets' cluster label `harbor.aetherize.io/cluster`,
   `harbor.aetherize.io/access-kind: nexus` (not `NexusAccess`), and
   `nexus.aetherize.io/nexusaccess-namespace` and `-name` (not under
   `harbor.aetherize.io/`). It does not carry
   `harbor.aetherize.io/managed-by`, so the Harbor janitor's Secret
   listing and the Harbor reconciler's Secret watch never see it. A
   Secret without the labels is never adopted: no Nexus Secret predates
   them.
2. **Secret contract package** (decisions e, g). The contract lives in
   `bridge/internal/nexussecret`, next to `robotsecret`, not inside it.
   The rotation promise reuses the robot Secret's annotation key
   `harbor.aetherize.io/rotation-not-before` with its ADR-0023 meaning.
   On top of the proposal the Secret carries
   `nexus.aetherize.io/user-id` (the user whose password it holds, equal
   to `username`; a Secret whose two values differ counts as incomplete),
   the pending generation as `nexus.aetherize.io/pending-user-id`, and
   `nexus.aetherize.io/retiring-user-id` with `retire-after` (point 4). A
   Secret created only to record a pending user has no data; the data
   plane must treat it like a missing Secret.
3. **Selector** (decision a, h). `BRIDGE_HARBORACCESS_SELECTOR` and
   `BRIDGE_INSTANCE` select NexusAccess objects too; there is no
   `BRIDGE_NEXUSACCESS_SELECTOR`. The finalizers are
   `nexus.aetherize.io/user` and `nexus.aetherize.io/user-<instance>` as
   proposed.
4. **The previous user outlives the Secret write by a grace**
   (decision c). A rotation creates the new generation, writes the Secret,
   and deletes the previous user `NexusUserRetireGrace` (5 minutes) later,
   recorded in the Secret, instead of right after the write. A scheduled
   rotation happens only after the old password's rotation-not-before,
   so no kubelet cache holds it any more; the grace covers a pull that
   fetched the old credentials just before and still uses the old user's
   docker bearer token (deleting the user refuses it mid-pull), and a
   data-plane replica whose cache has not yet seen the new Secret. It
   extends the lifetime of a leaked bearer token by as much. A forced
   rotation (Secret missing or incomplete, the named user gone) and a
   serviceAccountRef change delete the previous users at once, as the
   Harbor reconciler does.
5. **A refused NexusAccess** (not in the proposal; ADR-0030 parity).
   Refusal deletes the Secret and every active user of the object; it
   does not disable them, because a disabled user's bearer token is
   valid again once the user is re-enabled (Context 1), and resuming
   creates a user of a new generation anyway. A user an administrator
   disabled or locked stays, so that resuming reports `UserDisabled`
   instead of undoing that decision; the role stays too (it grants
   nothing without a user of the bridge). The finalizers are released
   once neither users nor a role of the object are left (ADR-0032). A
   user whose markers were edited out of band, while the Secret names it,
   is reported as `UserConflict` and neither used nor replaced.
6. **Registry hosts** (decisions f, h). `BRIDGE_HARBOR_REGISTRY_HOSTS` is
   optional and defaults to the host[:port] of `BRIDGE_HARBOR_URL`, also
   with Nexus enabled. `BRIDGE_NEXUS_REGISTRY_HOSTS` is required with
   Nexus; startup refuses a host[:port] both backends name. Harbor stays
   required (question 7). Any other `BRIDGE_NEXUS_*` setting without
   `BRIDGE_NEXUS_URL` fails startup.
7. **No backend seam yet** (decision g). The Nexus backend is its own
   controller and janitor (`NexusReconciler`, `NexusJanitor` in
   `bridge/controlplane`) that reuse the Harbor side's constants,
   resync spreading, selector, finalizer scheme and admin-credential
   reader. Extracting the seam and the conformance suite means
   restructuring the Harbor reconciler, which this change was not to
   alter; it is left to the maintainer's decision on g.
8. **Rate limit** (decision j). The backoff wraps the client both
   controllers share; the gauge is `bridge_nexus_rate_limited`. A
   NexusAccess being deleted reports `DeletionBlocked` with the backoff's
   explanation rather than `NexusRateLimited`.
9. **`format`** (decision a) is kept as proposed, with `docker` as its
   only value and default; `status.user.userId` is kept (question 9 stays
   open).
10. **`PasswordRejected`** is reported for a 400 to a user create whose
    message mentions the password; Nexus's validator message is not
    documented.
11. **Re-check of a missing repository** (decision d). Nexus sends no
    event when a repository is created, so an object in
    `RepositoryNotFound` is reconciled again after at most
    `NexusRepositoryRecheckInterval` (5 minutes) instead of the hourly
    resync.

The data plane and the entry point (2026-09-28) implement decision f and
the wiring of decision h's settings as follows.

12. **Routing** (decision f). Without `BRIDGE_NEXUS_URL` the handler
    does not route: every request is Harbor's and the image stays
    audit-only, byte for byte as before (responses, audit lines and
    metric series), except for a robot Secret that carries the
    access-kind label, which no bridge writes (note 13). With it, an
    image of a Nexus registry host is served from NexusAccess objects,
    an image of a Harbor registry host
    (`BRIDGE_HARBOR_REGISTRY_HOSTS`, by default the host of
    `BRIDGE_HARBOR_URL`) from HarborAccess objects exactly as before,
    and any other image, or a request without one, is refused with 403
    (`no_backend`). A request never falls back to the other backend. The
    parallel workstreams' interface contract phrased the second case as
    "otherwise the Harbor path runs", which would hand Harbor robot
    credentials to an image of a Nexus host outside its path prefix, or
    of a host no backend owns; this decision's `no_backend` rule is
    implemented instead. An image both backends claim, which startup
    refuses, is `no_backend` too. The backend is chosen before the token
    is checked, so every audit line names it (`access_kind`), and
    `no_backend` is decided after the check, so the refusal is
    attributed.
13. **Nexus refusals.** In the Harbor path's order: a NexusAccess being
    deleted, none matching, or one whose spec the reconciler refuses
    (an unparseable `tokenTTL`, no repositories) is 403; so is a Secret
    that is not `OwnedBy` the matched object, and one that carries
    `grants-incomplete`, whatever its value. A missing Secret, one
    without a complete password (a pending-only Secret), and one whose
    `username` differs from its `user-id` record (which the control
    plane treats as incomplete and replaces at once) are 503, as is one
    whose user id does not parse to the object's current identity
    (`secret_for_previous_identity`). The Harbor path additionally
    refuses any Secret that carries the access-kind label.
14. **Cache duration.** `rotation-not-before` caps kubelet's cache as
    for robot Secrets. A Nexus Secret without a parseable promise is
    served with a cache duration of 0, not the plain `tokenTTL` a legacy
    robot Secret gets: no Nexus Secret predates the promise, and the
    control plane replaces the user of such a Secret soon.
15. **Metrics.** The existing data-plane series keep their names and
    labels and count Nexus requests too. With the backend the bridge
    also exports `bridge_nexus_credential_issuances_total{result}` (the
    Nexus share, from the routing on), `bridge_nexusaccess_lookup_failures_total`
    and `bridge_nexus_user_secret_missing_total`; a Harbor-only bridge
    exports none of them.
16. **Scheme and caches.** The entry point adds `nexusv1alpha1` to the
    scheme, the NexusAccess cache index and `SetupNexus` only with the
    backend, so a Harbor-only bridge neither knows nor watches
    NexusAccess, and readiness waits for the NexusAccess cache
    (`Config.CachedObjects`) when it does.

The chart (2026-09-28) implements decision h as follows.

17. **Chart** (decision h). The values are the proposed ones, with
    `harbor.registryHosts` optional (point 6) and no selector of its own
    (point 3). Without `nexus.enabled` the chart's templates render
    nothing of Nexus, and `BRIDGE_HARBOR_REGISTRY_HOSTS` only when
    `harbor.registryHosts` is set, so a Harbor-only install renders as
    before; the CRD is another matter (note 20). The NexusAccess RBAC mirrors the HarborAccess rules (`get`,
    `list`, `watch`, `patch`; `update`, `patch` on the status; `update` on
    the finalizers): the control plane writes a NexusAccess only through
    its status and by patching its finalizers. With `nexus.enabled` the
    chart refuses a host[:port] that both backends name, and, with the
    chart-managed plugin, any registry host of either backend (Harbor's
    too, also when it defaults to the host of `harbor.url`) that no single
    `plugin.matchImages` entry covers under kubelet's `URLsMatch`: the
    same port, as many host labels, each matched by `filepath.Match`, and
    the entry's path a raw string prefix of `/<path prefix>`. So
    `host/nexus/` does not cover the entry `host/nexus`, whose image
    `host/nexus` the bridge routes to Nexus. The chart does not evaluate
    `[...]` classes or `\` escapes in a matchImages host and never counts
    such an entry as covering: it may report a gap kubelet would not
    have, never miss one. Since main's #142 the chart refuses a
    matchImages entry that is not `host[:port][/path]` (a path glob, a
    `?`, a character class or an escape) before it checks coverage;
    the rule still keeps a bracketed IPv6 address without a port, which
    kubelet reads as a character class, from covering anything. It does
    not check the reverse, a matchImages
    entry that no backend serves (the bridge refuses such images as
    `no_backend`): an entry with a path prefix could never pass it,
    because kubelet's `host/nexus` also matches `host/nexus-old/app`.

The e2e harness (2026-09-28) implements decision i as follows.

18. **e2e** (decision i). `tests/03-nexus.tftest.hcl` covers every
    assertion decision i lists, on Nexus 3.76.1, next to Harbor. How it
    departs from or adds to the proposal:
    - It runs only through `make e2e-nexus`. `make e2e`, `e2e.yml` and
      `harbor-compat.yml` filter `tofu test` to `01-plan` and
      `02-bridge`, whose runtime stays as it was; whether CI runs the
      Nexus harness is left to the maintainer.
    - `test/e2e/nexus/Dockerfile` pins the image by digest and copies its
      filesystem into an image of the build platform: containerd's CRI
      plugin refuses an image of another platform even after
      `kind load`, and the image is linux/amd64 only. On arm64 the
      binaries run through binfmt (Rosetta). `renovate.json` holds the
      pin below 3.77.
    - The docker connectors speak plain HTTP; containerd reaches them
      through `hosts.toml` with an `http://` server, so no TLS front.
      Each hosted repository has its own connector and NodePort (3.76
      has no path routing).
    - The bridge authenticates as a user with a random name holding only
      `nx-users-all`, `nx-roles-all` and `nx-privileges-read` (decision
      j), so every stage proves those suffice.
    - The rotations are triggered through the Secret: a backdated
      `rotation-not-before` (scheduled rotation, previous user retired
      after `NexusUserRetireGrace`) and a deleted Secret (forced
      rotation, on the object in `RepositoryNotFound`).
    - "Nexus is down" is Nexus scaled to zero on its PVC. A NetworkPolicy
      or an emptied Service would leave the client's idle keep-alive
      connections to Nexus open.
    - Routing also asserts the refusal of an image of neither backend
      (`no_backend`, note 12): the Nexus connectors' host on a port no
      registry host names, asked of the bridge directly, since kubelet
      calls the plugin only for images `matchImages` covers.

The implementation review (2026-09-28) changed the following.

19. **A privilege counts only as Nexus's own** (decisions b, d). The
    reconciler grants a repository only when every privilege it reads
    back under the repository's repository-view names is Nexus's built-in
    privilege of it: type `repository-view`, format `docker`, that
    repository, exactly the one action the name ends in (upper-case), and
    read-only (`nexus.VerifyRepositoryPrivilege`). While a repository does
    not exist, anyone holding `nx-privileges-create` can create a
    privilege under its name, of any type, for example a wildcard
    privilege with the pattern `nexus:*` (verified). A check by name
    alone wrote it into the role, and every credential of the identity,
    whose password reaches nodes, held it. Such a repository is not
    granted and counts as missing for the Secret's `grants-incomplete`
    mark; the object reports `PrivilegeConflict` (a reason decision a
    does not list), which an administrator resolves by deleting the
    privilege, and is checked again every `NexusRepositoryRecheckInterval`.
    Once the repository exists, Nexus reports its built-in privilege under
    the name again (verified).
20. **The CRD's lifecycle** (decision h). The NexusAccess CRD lives in the
    chart's `crds/`, next to HarborAccess's. Helm creates the CRDs there
    on `helm install` only, whatever the values, and never on `helm
    upgrade`, nor deletes them on uninstall. So a fresh Harbor-only install
    gets the CRD, inert without the backend, and enabling Nexus on an
    existing release needs the CRD applied by hand (README, MIGRATION.md,
    and the NOTES of an upgrade with `nexus.enabled` say so). Without it
    the manager cannot build its NexusAccess cache and the bridge exits at
    startup; the error names the CRD and the file to apply
    (`bridge/cmd/main.go` `newManager`). Rendering the CRD as a template
    gated by `nexus.enabled` would install it on upgrade, but `helm
    uninstall` or turning the flag off would then delete it, which deletes
    every NexusAccess object while no bridge with the backend is left to
    revoke their users and release their finalizers.
21. **A path prefix selects requests, it does not confine credentials**
    (decision f). The data plane matches a path prefix on `/` boundaries
    and refuses `host/nexus-old/app` for the entry `host/nexus`, but every
    response carries `cacheKeyType: Registry` (ADR-0016) and the plugin
    keys the credentials by the bare host (`plugin/main.go`
    `writeOKResponse`). Kubelet looks up its cache by image, then by
    registry host, so once it holds credentials for an image of a
    host[:port], it uses them for every image of that host[:port] its
    `matchImages` covers until the cache duration ends, without asking
    the bridge. Backends stay apart, since two may not
    share a host[:port]; a service behind the same host[:port] outside the
    prefix does not. README and values.yaml say so. Answering a request
    that matched only through path-prefixed entries with `cacheKeyType:
    Image` would confine the credentials to the image, at the price
    ADR-0016 rejected `Image` for: one plugin run and bridge request per
    image and node instead of per registry host. That is left to the
    maintainer.

## Verified at runtime

Nexus `sonatype/nexus3:3.76.1` in a local container bound to 127.0.0.1,
2026-09-28, `curl` and `crane` against the REST API and a hosted docker
repository with an HTTP connector, `DockerToken` realm active, anonymous
access off; removed afterwards. The review of this ADR repeated the
rows marked (r) in a fresh container of the same image the same day; the
implementation review added the rows marked (i), again in a fresh
container of the same image.
The last column says whether `TestLive_AgainstNexus` checks the row.

| Behaviour | Result | Live test |
| --- | --- | --- |
| Duplicate user id on create | 500, text/plain `DuplicateUserException: User … already exists.` | yes |
| Duplicate role id on create | 400, `Role '…' already exists, use a unique roleId.` | yes |
| Role update with body id ≠ path id | 409 | no |
| Role create with a privilege that does not exist | 400 | yes |
| Role update with a privilege that does not exist | 400, nothing stored (r); 3.91.0+ stores the role without it and answers 204 (source only) | yes |
| User update with an unknown role | 400, validation list `[{"id":"roles","message":…}]` | yes |
| User update with body id ≠ path id | 400 | no |
| User create with an unknown role (r) | 200; the answer echoes the request; reads hide the role; the user gains it once a role with that id is created | all but the echo |
| User create without roles / without e-mail | 400 each | no |
| Two users with the same e-mail | allowed | yes |
| User id of 200 / 201 characters | 200 / 500 | yes |
| `--`, `_` and a trailing `.json`/`.xml` in user ids | accepted; PUT and DELETE address the right user | `_` only |
| `GET /v1/security/users?userId=bridge-rt.` | also returns `BRIDGE-RT.ns.upper`: case-insensitive prefix | no (only the client's filter) |
| `DELETE …/users/{id}?realm=NexusAuthenticatingRealm` of a missing user | 404 | no |
| 404 for a missing user (PUT, DELETE), role (GET, PUT, DELETE) or privilege (GET) (r) | JSON `{"id":"*","message":"\"… not found.\""}` | 404 only |
| Deleting a role (r) | removes it from every user; re-creating it does not give it back | first half |
| Deleting a repository | strips its privileges from every role; re-creating it does not restore them | no |
| `GET /v1/security/privileges/{name}` | 200 for an existing repository, 404 otherwise; names case-sensitive | yes |
| `GET /v1/security/privileges/nx-all` (r) | 200, `readOnly: true` | 200 only |
| Built-in repository-view privilege (i) | `type: repository-view`, `format: docker`, `repository: <name>`, one upper-case action (`actions: ["READ"]`), `readOnly: true` | yes |
| Wildcard privilege created under the repository-view name of a repository that does not exist (i) | 201; `GET` of the name returns it (`type: wildcard`, `readOnly: false`) until the repository is created, then the built-in one; a role naming it is stored | the create and its refusal |
| Custom repository-view privilege of a repository that does not exist (i) | 400, `Invalid repository '…' supplied.` | no |
| `change-password` | 204 with text/plain, 415 with application/json | no |
| Request without credentials / wrong password | 403 / 401 | yes |
| `/v1/status`, `/v1/status/writable` without credentials | 200 | yes |
| Admin role of `nx-users-all`, `nx-roles-all`, `nx-privileges-read` only (r) | every client call succeeds | yes |
| Bearer token after password change | still valid; `/v2/token` returns the same token | no |
| User in status `changepassword` (r) | Basic Auth, `/v2/token` (same token) and the existing bearer token keep working | yes |
| Bearer token while disabled or locked / after re-enabling | 401 / valid again | disabled only |
| Bearer token after delete and re-create under the same id | still valid (0 s, 10 s and 30 s gap); the new user gets the same token | 0 s only |
| Bearer token presented while its user is absent | 401, and stays 401 after re-creation; the re-created user gets a new token | yes |
| Bearer token after replacing the user under a new id | old token 401, new token 200 | yes |
| Least privilege (crane) | pull, tag list, catalog: `read`; push: `add`+`edit`+`read`; `add`+`browse`+`read` cannot push | no |
| Generated passwords (`GeneratePassword`) | authenticate for the REST API and `/v2/token` | yes |
| `oci` format / EULA endpoint | absent (404) on 3.76.1 | no |

`TestLive_AgainstNexus` passed against both containers.

## Consequences

- A second registry is supported with the same identity model and the
  same rotation promise as Harbor, and for docker repositories the same
  revocation guarantees, bounded by rotation and a 5-minute grace
  (decision c, note 4) as long as the control plane can authenticate to
  Nexus. The price: a second CRD, a routing step in the data plane and a
  backend seam in the control plane.
- Nexus user ids change at every rotation; the role id, the Secret name
  and the NexusAccess stay. Each identity has one user, and two for up to
  `NexusUserRetireGrace` (5 minutes) after each scheduled rotation (note
  4); a forced rotation and a serviceAccountRef change delete the
  previous user at once.
- A docker bearer token leaked from a node stays valid until the next
  rotation or deletion retires its user, not until its own expiry, which
  Nexus does not have: while the admin credential works, at most
  `PasswordRotationInterval + RotationSafetyMargin + NexusUserRetireGrace`
  (24 h + 1 min + 5 min) after the user was created, plus the reconcile
  latency. The rotation happens once `rotation-not-before` plus the
  margin has passed, and the replaced user is deleted after the grace.
- Orphan API keys of retired users stay in Nexus's store until presented
  (then deleted) or purged; they grant nothing, because their user ids
  never exist again.
- A missing repository never widens a grant and never holds back a
  removal; it stops new credentials for that NexusAccess until it exists
  again or leaves the spec.
- The bridge holds an admin-equivalent Nexus credential whose username
  must not be guessable: from Nexus 3.94 on anyone who can reach Nexus
  and knows it can suspend rotation and revocation. SECURITY.md and the
  threat model must say so.
- `oci` repositories are not supported until question 8 is answered.
- The e2e covers only pre-3.77 releases unless an operator accepts the
  EULA; behaviour specific to later releases stays unverified.
- A Harbor-only installation renders and behaves as before; a fresh
  `helm install` creates the NexusAccess CRD too, inert without the
  backend. Enabling Nexus on an existing release needs that CRD applied by
  hand, since `helm upgrade` creates no CRDs (note 20).
