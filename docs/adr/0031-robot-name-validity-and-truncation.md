# 31. Robot names Harbor refuses, and what keeps truncated names apart

## Status

Accepted (2026-09-28). Amends [ADR-0018](0018-dot-delimited-naming.md):
it replaces ADR-0018's statements that the trailing `saName` field may
take dots, that every `bridge-<cluster>.<ns>.<sa>` is a valid Harbor
robot name, and that the overflow path rests on the hash alone. The
dot-delimited scheme itself is unchanged.

## Context

A security audit (2026-09, Harbor client findings 23 and 78) found two
claims in ADR-0018 that do not hold.

**Harbor validity.** ADR-0018 says `bridge-<cluster>.<ns>.<sa>` is valid
for Harbor's robot regex `^[a-z0-9]+(?:[._-][a-z0-9]+)*$` (Harbor's
`validateName`, `src/server/v2.0/handler/robot.go`, unchanged since robot
accounts v2 in Harbor 2.2). The regex allows only single separators. The
cluster name and the ServiceAccount namespace and name are DNS labels,
which may contain consecutive hyphens (`team--a`, punycode `xn--…`).
Harbor answered `POST /robots` for such an identity with 400. The
reconciler classified that as a transient Harbor error, so the
HarborAccess retried with backoff forever as `HarborError`.

**Truncation.** When the natural name exceeds `RobotNameCap` (240),
`RobotName` keeps `bridge-<cluster>.`, cuts `<ns>.<sa>` to fit, and
appends `.` plus the first 16 hex characters of the SHA-256 of the
natural name. A truncated name therefore has three dots after `bridge-`,
a natural name two. The two sets are disjoint only while:

- `saName` is dot-free. A dotted `saName` gives a natural name the shape
  of a truncated one. The digest is an unkeyed hash of public inputs, so
  anyone who may name a ServiceAccount could choose the name whose natural
  robot name equals another identity's truncated name. ADR-0018 says the
  trailing field "may contain dots freely" and that relaxing `saName` to
  a DNS subdomain would be safe; for truncated names it is not.
- the cut falls inside the SA name. With a cluster name of at most 63
  characters (config) and a namespace of at most 63 (Kubernetes),
  `bridge-<cluster>.<ns>.` takes at most 135 characters, fewer than the
  223 that fit before `.<digest>`. Only a `serviceAccountRef.namespace`
  longer than any namespace can be (the CRD allows 253) cuts inside the
  namespace, and no token can come from such a namespace.

Between two truncated names injectivity rests on the 64-bit digest, as
before.

## Decision

1. `RobotName` checks its result against Harbor's regex and returns
   `ErrInvalidRobotName` for an identity Harbor cannot name. The
   reconciler maps it to the terminal `InvalidSpec`. The janitor treats
   an owner whose identity maps to no valid name as wanting no robot, so
   editing an identity to such a name still revokes the previous robot.
   Every name Harbor accepts stays the same.
2. `BRIDGE_CLUSTER_NAME` must not contain `--`. The cluster name is part
   of every robot name, so such a bridge could create no robot; it now
   refuses to start.
3. `saName` stays dot-free. Relaxing the CRD pattern of
   `serviceAccountRef.name` to DNS subdomains first requires joining the
   digest with a character no DNS name contains, such as `_` (Harbor's
   regex allows it as a separator). That renames every truncated robot,
   so it needs a migration and its own ADR.
4. The naming guarantee reads: natural names are injective by the dot
   delimiter (ADR-0018); truncated names are disjoint from natural names
   under the two conditions above; two truncated names differ by their
   digest.

## Consequences

**Positive:**

- A HarborAccess whose identity Harbor cannot name reports `InvalidSpec`
  once, with the reason, instead of retrying as `HarborError`.
- A cluster name no robot could carry fails at startup.
- The decision log states the conditions the naming guarantee depends
  on, so a later relaxation of `saName` cannot rely on ADR-0018's
  statement that it is safe.

**Negative:**

- A ServiceAccount whose namespace or name contains `--` cannot get a
  robot. It never could: Harbor refused the name. Mapping such
  identities to a Harbor-valid name (escaping or hashing them) would
  keep them working but adds a second naming path whose disjointness
  from the natural names needs its own proof; that is left to a separate
  decision.

Pinned by `TestRobotName_RejectsNamesHarborRefuses`,
`TestRobotName_TruncatedNamesAreDisjointFromNaturalNames`,
`FuzzRobotName_Injective` (every returned name is Harbor-valid),
`TestReconcile_IdentityHarborCannotNameIsInvalidSpec`,
`TestJanitor_RevokesRobotWhenOwnerMapsToNoValidName` and the
"cluster name with consecutive hyphens" case of
`TestLoadFromEnv_ValidationErrors`.

## Alternatives considered

- **Keep retrying.** Harbor's refusal is permanent; retries only hide
  the reason behind `HarborError`.
- **Escape or hash identities with `--`.** See Consequences; not decided
  here.
- **Join the digest with `_` now.** Makes a dotted `saName` safe, but
  renames every truncated robot while the CRD still forbids dots, for no
  present gain.
