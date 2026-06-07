# 9. Multi-cluster topology

## Status

Accepted. **The robot-naming scheme (§2) and the hyphen-prefix ownership
caveat (§3) are superseded by [ADR-0018](0018-dot-delimited-naming.md)**, which
switches the field delimiter from `-` to `.`. The names below now read
`bridge-<cluster>.<saNs>.<saName>`, the ownership prefix is `bridge-<cluster>.`,
and the "cluster names must not be hyphen-prefixes of each other" operator
burden is removed. The rest of this ADR (bridge-per-cluster, ownership as a
safety invariant, per-cluster admin creds, issuer-mismatch detection) stands.

## Context

Many real-world Harbor users run **N Kubernetes clusters against 1 Harbor instance** (production / staging / dev clusters all pulling from the same registry, or geographic separation like `prod-eu-west` / `prod-us-east`). The bridge has to support this from day one — retrofitting cluster scope into robot names and reconciler ownership later would be a breaking change for every operator and a hazard for shared Harbor instances during the transition.

This decision is about how cluster scope is represented and enforced inside the bridge.

**Scenarios in scope for this iteration:**

- N Kubernetes clusters → 1 Harbor instance.
- Each cluster runs its own bridge deployment.
- Bridges have no shared state and do not talk to each other.
- A single Harbor instance hosts robots produced by every connected bridge, distinguished only by name prefix.

**Scenarios explicitly out of scope:**

- 1 cluster → N Harbor instances. A single bridge fronts one Harbor.
- Shared robots across clusters (e.g. one robot serving identical workloads on prod-eu-west and prod-us-east). Each cluster gets its own robot.
- Central bridge instance with multiple kubeconfig contexts.
- Cross-cluster RPC, consensus, leader election, or shared etcd.
- Auto-discovery of cluster identity from kubeconfig context or node labels.

## Decision

**Bridge per cluster, autonomous, no shared state.** Coordination across bridges happens implicitly via a strict robot-naming convention, not via any runtime mechanism.

### 1. Cluster identity is bridge configuration

Each bridge deployment is configured with a required `clusterName`:

- Source: Helm value `clusterName`, plumbed into the deployment as env `BRIDGE_CLUSTER_NAME`.
- Validated as a DNS label: regex `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars.
- Read once at startup; no hot-reload.
- The Helm chart's `required` helper fails install when the value is empty. No defaults — silent ambiguity is the failure mode we are most worried about.

### 2. Robot naming carries the cluster prefix

Robot username: `bridge-<clusterName>.<saNamespace>.<saName>` (dot-delimited
per [ADR-0018](0018-dot-delimited-naming.md); originally `-`-delimited, which
was not injective — see that ADR).

- Deterministic from the `HarborAccess` CR (its `serviceAccountRef`) plus the bridge's configured `clusterName`. Reconciles are idempotent.
- Names exceeding Harbor's robot-username cap are deterministically truncated: keep the `bridge-<clusterName>.` prefix, then append a `sha256(full-name)` suffix truncated to fit. The exact cap is verified against `goharbor/go-client` constants at code-time, not assumed.
- Example: `bridge-prod-eu-west.flux-system.source-controller`.

### 3. Ownership filter as a safety invariant

A bridge **must never list, modify, or delete a robot whose name does not begin with `bridge-<clusterName>-`**.

- Single helper: `ownsRobot(name string) bool { return strings.HasPrefix(name, "bridge-"+r.clusterName+"-") }`.
- Applied at every call site touching Harbor robots: reconciler create/update, reconciler delete via finalizer, janitor orphan sweep.
- Enforced in unit tests for every site, including adversarial cases (a CR crafted to produce a name colliding with another cluster's prefix is rejected before reaching Harbor).

This is a safety invariant, not an optimisation. A bridge bug in one cluster must not be able to delete or modify robots produced by another cluster's bridge against the same Harbor.

**~~Operator-side caveat: cluster names must not be hyphen-prefixes of each other.~~ — Resolved by [ADR-0018](0018-dot-delimited-naming.md).** The original ownership prefix `bridge-<cluster>-` produced a false positive: `strings.HasPrefix(robotName, "bridge-prod-")` returned true for cluster `prod-eu`'s `bridge-prod-eu-…` robots, which the bridge could not disambiguate from the name alone — pushing "choose cluster names so none is a hyphen-prefix of another" onto operators. ADR-0018 adopted the `.` separator this paragraph anticipated: the ownership prefix is now `bridge-<cluster>.`, and because `cluster` is a dot-free DNS label, `bridge-prod.` is *not* a prefix of `bridge-prod-eu.…`. The class is closed in software; the operator burden is gone. The fix is pinned by `TestOwnsRobot_DotDelimiterFixesPrefixCollision`.

### 4. The CRD does not carry cluster identity

`HarborAccess` is namespaced and cluster-local by definition; carrying `clusterName` inside the CR would either be redundant (when it matches the bridge's config) or contradictory (when it doesn't), with no useful resolution rule for the contradiction case. The bridge is the only authority on its own cluster's identity.

### 5. Harbor admin credentials are per-cluster

Two supported modes; the chart shape is identical:

- **Per-cluster system robot (recommended for production).** Each bridge holds credentials for a distinct Harbor system robot with `create-robot` + `delete-robot` permissions. Compromise of one cluster's secret leaks only that cluster's Harbor admin surface.
- **Shared Harbor admin user.** Simpler day-one setup. Compromise of any cluster's secret leaks Harbor admin across all connected clusters.

Harbor does **not** natively scope a system robot's `create-robot` permission to a name prefix. The ownership filter (decision 3) remains the only mechanism preventing one bridge from acting on another cluster's robots. Per-cluster credentials reduce blast radius but do not change the software invariant.

### 6. Issuer mismatch is detected at reconcile time

A bridge is configured with one OIDC issuer (its cluster's). If a `HarborAccess` CR's `spec.trustPolicy.issuer` does not match the bridge's configured issuer, the reconciler sets `Ready=False` with reason `IssuerMismatch` and stops without touching Harbor. This catches the "applied a CR meant for cluster A into cluster B" misconfiguration loudly and early.

## Consequences

**Positive:**

- Architecture stays simple. No distributed state, no leader election, no IPC between bridges.
- Blast radius is isolated per cluster. A misbehaving bridge cannot corrupt robots managed by another cluster's bridge (enforced by software, not policy).
- Cluster onboard is one Helm install with a fresh `clusterName`. Cluster offboard is `delete all Harbor robots with prefix bridge-<clusterName>-*` plus the cluster's bridge.
- Harbor audit logs reveal cluster of origin via the robot prefix. Forensic analysis after an incident is straightforward.
- Upgrades are per-cluster. No rolling upgrade across clusters required, no coordination needed.

**Negative / trade-offs accepted:**

- The same workload pattern (same SA name in the same namespace) running across N clusters produces N robots in Harbor. No deduplication. Operators must size Harbor and audit-log retention with this in mind.
- Per-cluster Harbor admin credentials are recommended for blast-radius reasons; clusters that share a credential trade isolation for setup simplicity.
- Operator error changing `clusterName` between bridge restarts orphans the bridge's old robots (they belong to the previous `clusterName`). The new bridge will not adopt them — by design, per decision 3. Cleanup is manual and documented in `README.md`.

**Forward implications:**

- After upstream Harbor lands OIDC trust policies ([goharbor/harbor#17520][upstream]) and the data plane retires (see [ADR-0002](0002-bridge-control-plane-data-plane-split.md)), JWKS access becomes a per-cluster concern: Harbor must be able to fetch each connected cluster's JWKS endpoint. Air-gapped or private-API-server clusters keep `forceLocalValidation=true` and continue routing through the bridge data plane indefinitely. ADR-0006 covers the local-validation path; a future ADR will cover the post-migration path when concrete.
- The control-plane reconciler shape does not change at migration: it still owns one robot per CR, scoped by `clusterName`. The only addition is writing trust-policy fields onto the Harbor robot once Harbor accepts them.

## Alternatives considered

- **Central bridge with multiple kubeconfig contexts.** Rejected. A single point of failure for all clusters. JWKS access from a bridge outside the cluster requires either a publicly reachable issuer or a complex pull-through architecture. Adds cross-cluster credential plumbing without solving any user problem we have evidence for.
- **Bridge-per-cluster with shared robots via a consensus protocol.** Rejected. Buys ~zero benefit (a shared robot is no more pull-throughput than a per-cluster robot, and Harbor's robot count is not a constraint anyone reports hitting), at the cost of operating Raft / etcd / similar across bridges. The fragility–value ratio is upside-down.
- **Cluster identity carried in the CRD.** Rejected. Either redundant with the bridge's own config (cluster-local CR + cluster-local bridge always agree) or contradictory with no good resolution rule. Adds a footgun and a validation surface for nothing.
- **Automatic cluster-name detection** (kubeconfig context, node labels, kube-system namespace UID, etc.). Rejected. Magic that diverges silently across environments. Explicit configuration is the failure mode we want: an operator who forgets to set `clusterName` sees the install fail immediately, not a bridge happily naming robots `bridge-default-...` and colliding with another cluster.

[upstream]: https://github.com/goharbor/harbor/issues/17520
