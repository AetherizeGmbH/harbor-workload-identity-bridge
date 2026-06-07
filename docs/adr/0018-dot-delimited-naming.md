# 18. Dot-delimited robot and Secret names for collision-free identity mapping

## Status

Accepted (supersedes the naming scheme and the hyphen-prefix operator
caveat of [ADR-0009](0009-multi-cluster-topology.md) §2–§3)

## Context

Two names encode workload identity in this system, and both were built by
**dash-joining** fields that may themselves contain dashes:

- Harbor robot name: `bridge-<cluster>-<saNamespace>-<saName>`
  (`controlplane/harbor/naming.go`).
- Robot-password Secret name: `robot-<haNamespace>-<haName>`
  (`controlplane/reconciler.go` and its data-plane mirror `dataplane/handler.go`).

A `-` join over fields that allow `-` is **not injective**. Distinct
identities collapse to the same string:

```
RobotName(c, "team-a", "svc")        == RobotName(c, "team", "a-svc")        == "bridge-c-team-a-svc"
secretNameFor(ns="team-a", n="svc")  == secretNameFor(ns="team", n="a-svc")  == "robot-team-a-svc"
```

A security audit (AUDIT.md F2) showed this is a multi-tenancy boundary break:
a HarborAccess author can craft a `(namespace, name)` that collides with another
CR's Secret name and have one workload's ServiceAccount served another's robot
credentials (last-writer-wins on the shared Secret), or collide on the robot
name and have two CRs fight over one Harbor robot (permission overwrite +
password-rotation DoS).

Runtime **guards** were added first (refuse-on-collision in the reconciler, a
403 backstop in the data plane). Those make a collision fail *safe* (deny) but
do not let two legitimately-distinct workloads coexist when their names collide.
This ADR fixes the root cause: an injective naming scheme.

A separate, long-standing defect lives in the same code: ADR-0009 §3's ownership
prefix `bridge-<cluster>-` produces a **false positive** when one cluster name
is a hyphen-prefix of another (`bridge-prod-` matches cluster `prod-eu`'s
`bridge-prod-eu-…` robots), which ADR-0009 pushed onto operators as "cluster
names must not be hyphen-prefixes of each other." ADR-0009 itself flagged `.` as
the eventual fix. This ADR adopts it.

The system is pre-release (alpha; `make e2e` builds a fresh kind + Harbor each
run). **There is no deployed state to migrate** — no live robots in any shared
Harbor, no `robot-*` Secrets in any cluster — so the scheme can change in place.

## Decision

Join the identity fields with `.` instead of `-`.

- Robot name: **`bridge-<cluster>.<saNamespace>.<saName>`**.
- Secret name: **`robot-<haNamespace>.<haName>`**.
- Ownership prefix: **`bridge-<cluster>.`** (dot-terminated).

The hash-on-overflow fallback is unchanged: when the natural name exceeds
`RobotNameCap`, the `bridge-<cluster>.` prefix is preserved, the `ns.sa` portion
is truncated, and a SHA-256 suffix of the full pre-truncation name disambiguates.
That truncation path is the *only* place where injectivity rests on the hash
rather than the delimiter.

### Why `.` is injective here — the invariant

A dot-joined name `A.B.C` is injective **iff every field except the last is
dot-free**. The first dot then unambiguously ends `A`, the second ends `B`, etc.
This holds for both names because every field left of the last dot is a
Kubernetes namespace or the cluster label, none of which may contain a dot:

| Name   | Fields (left → right)                | Dot-free guarantee on the left fields |
|--------|--------------------------------------|----------------------------------------|
| Secret | `<haNamespace>` . `<haName>`         | `haNamespace` is a namespace → RFC 1123 **label**, no dots |
| Robot  | `<cluster>` . `<saNamespace>` . `<saName>` | `cluster` (config DNS label) and `saNamespace` (namespace) → no dots |

The **trailing** field may contain dots freely without breaking the split
(`robot-ns.my.app` recovers as `ns` / `my.app`). This is a useful asymmetry: a
future relaxation that lets `saName`/`haName` carry dots (real Kubernetes
ServiceAccount names are DNS subdomains and *do* allow dots — the CRD is
currently stricter) would **not** reintroduce the collision.

Validation that backs the invariant today:

- `cluster` — `BRIDGE_CLUSTER_NAME`, validated `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
  (config.go). No dots.
- `saNamespace`, `haNamespace` — Kubernetes namespaces. RFC 1123 labels. No dots
  (a hard apiserver guarantee, independent of the CRD).
- `saName` — CRD pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (no dots today), but
  safe even if relaxed because it is the trailing field.

Both `bridge-<cluster>.<ns>.<sa>` (Harbor robot regex
`^[a-z0-9]+([._-][a-z0-9]+)*$`) and `robot-<ns>.<name>` (a DNS subdomain) are
valid in their respective systems.

### Bonus: the ownership prefix becomes collision-free too

Because `cluster` is dot-free, the dot-terminated ownership prefix
`bridge-<cluster>.` is non-prefixing across distinct cluster names:
`bridge-prod.` is **not** a prefix of `bridge-prod-eu.flux.svc` (the char after
`bridge-prod` is `-`, not the required `.`). This retires ADR-0009's
"cluster names must not be hyphen-prefixes of each other" operator burden. The
description-tag check (`RobotBelongsToCluster`, ADR-0012) remains as
defense-in-depth.

## Consequences

**Positive:**

- Robot and Secret names are provably injective over all valid inputs (the
  overflow path remains hash-bounded). The F2 collision class is closed at the
  root, not just guarded.
- The ADR-0009 hyphen-prefix ownership false-positive is eliminated; operators
  no longer have to reason about hyphen-prefix relationships between cluster
  names.
- Names stay human-readable and greppable (`bridge-prod-eu-west.flux-system.svc`),
  unlike a pure-hash scheme.

**Negative / trade-offs:**

- The naming scheme is a contract documented in ADR-0009, the README, and
  SECURITY.md; this changes it. Acceptable now precisely because nothing is
  deployed — **post-release this would be a breaking rename requiring a robot/
  Secret migration.**
- The injectivity guarantee is *contingent* on the dot-free invariant for all
  non-trailing fields. If a future change relaxed a namespace or the cluster
  field to allow dots, collisions would return. Pinned by
  `TestRobotName_DotDelimiterIsInjective` and
  `TestSecretNameFor_DotDelimiterIsInjective`.
- The runtime collision guards from the F2 fix are kept as belt-and-suspenders
  (they now only fire on the hash-overflow tail or a future invariant
  regression).

## Alternatives considered

- **Always append a hash suffix** (`<readable>-<sha>`). Robust to any charset
  change, reuses existing overflow machinery, but only *probabilistically*
  injective (64-bit birthday bound) and less readable. For a security boundary,
  the dot scheme's *provable* injectivity is preferable; the hash stays only as
  the length-overflow fallback.
- **Key the Secret by `HarborAccess` UID.** Unique by construction, but opaque
  (bad operability), changes on CR delete/recreate, and does not solve the robot
  name (the robot is keyed by SA-ref, which has no UID). Would force two schemes.
- **Validating admission webhook that rejects colliding CRs.** No rename, but
  refuses one of two colliding workloads rather than letting both coexist, and
  adds webhook operational cost. This is the apply-time form of the runtime
  guards we already have, not a root-cause fix.
- **Keep dashes, forbid dashes in inputs.** Impossible — real namespaces and SA
  names contain dashes.

[adr-0009]: 0009-multi-cluster-topology.md
