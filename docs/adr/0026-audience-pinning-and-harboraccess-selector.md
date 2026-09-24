# 26. One bridge serves one audience and a selected set of HarborAccess objects

## Status

Accepted. Extends ADR-0010 (identity), ADR-0017 (audience RBAC) and
ADR-0023 (lifecycle).

## Context

- The bridge accepted whatever audience a HarborAccess named in
  `spec.trustPolicy.audience`. The chart already configures one audience
  (`plugin.audience`): kubelet requests tokens for it, and the audience RBAC
  of ADR-0017 allows only it. A CR author could still name another
  audience, for example the apiserver's default one. Then every automounted
  token of that ServiceAccount, including tokens leaked to third-party
  webhooks, was redeemable for Harbor credentials (audit 2026-09, M7).
- A cluster can run several bridges, for example one per Harbor instance.
  Every bridge reconciled every HarborAccess, so each CR got a robot in every
  Harbor, and every bridge answered for every CR.

## Decision

1. **Pinned audience.** `BRIDGE_AUDIENCE` (chart: `plugin.audience`) is
   required. A HarborAccess whose `trustPolicy.audience` differs gets
   `Ready=False, reason=AudienceMismatch` and no robot. The data plane
   issues credentials only for tokens that carry this audience, and only
   for CRs that name it.
2. **HarborAccess selector.** `BRIDGE_HARBORACCESS_SELECTOR` (chart:
   `bridge.harborAccessSelector`, a `matchLabels` map) is a Kubernetes
   label selector. Empty means every HarborAccess (the behaviour so far).
   Otherwise the bridge's cache holds only matching objects: the
   reconciler and the data plane never see the others.
3. **Per-instance finalizer.** With a selector the bridge uses the
   finalizer `harbor.aetherize.io/robot-<instance>` (`BRIDGE_INSTANCE`,
   chart: `bridge.instance`, default the release name) instead of the
   shared `harbor.aetherize.io/robot`. When it deletes a CR it manages it
   also removes the shared finalizer, which only a pre-selector
   installation can have set.
4. **A CR that stops matching is released.** The janitor reads owners
   uncached (ADR-0023). When a robot's HarborAccess still exists but no
   longer matches the selector, it deletes the robot and the robot Secret
   (revocation) and removes its own instance finalizer. Moving a CR to
   another bridge by relabelling is therefore safe: the new bridge adds its
   own finalizer and robot, the old one revokes.
5. **Bridges that share a Harbor must use different `clusterName`
   values.** The cluster name is the robot ownership prefix (ADR-0009,
   ADR-0018). Two bridges with the same prefix in one Harbor would treat
   each other's robots as their own.

## Consequences

- A CR with another audience stops working on upgrade; it is reported as
  `AudienceMismatch` (MIGRATION.md). Fix the CR's audience.
- Selectors of different bridges may overlap; an overlapping CR then gets a
  robot from each bridge whose audience it names. Because a CR names one
  audience, only one bridge can serve it unless the bridges share an
  audience.
- On the nodes, one chart-installed plugin per node is still the limit: the
  plugin's provider name and file names are fixed, so a second plugin
  DaemonSet replaces the first one's kubelet entry, and one plugin talks to
  one bridge. A second bridge on the same cluster needs its plugin installed
  out of band (`plugin.enabled=false`) as a copy of the binary under another
  name, with its own provider entry (kubelet runs the binary named like the
  entry). Chart-managed plugins per bridge need configurable provider names
  (follow-up).
