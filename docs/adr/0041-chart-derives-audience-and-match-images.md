# 41. The chart derives the audience and matchImages from the required values

## Status

Accepted, 2026-11-08. Refines ADR-0026 (one audience per bridge) and
ADR-0021 (the plugin's kubelet entry) for the chart's defaults.

## Context

- An install needs six values: `clusterName`, `harbor.url`,
  `harbor.adminCredsSecret.name`, `plugin.audience`, `plugin.matchImages`
  and `tls.issuerRef.name`. Two of them repeat what the others already say:
  - `plugin.audience`: values.yaml and the README recommend
    `harbor-bridge-<clusterName>`, so that a token minted for one cluster's
    bridge does not work on another cluster that uses the same Harbor. Every
    user types that string by hand.
  - `plugin.matchImages`: in almost every install it is the host of
    `harbor.url`, the registry the pods pull from.
- Bridges on one cluster should use distinct audiences: a HarborAccess
  names one audience, so the audience decides which bridge serves it, and
  only bridges that share an audience can both serve one object
  (ADR-0026). Bridges on one cluster that share a Harbor must use distinct
  `clusterName` values (ADR-0026 decision 5); bridges on different Harbors
  may share one.
- Kubelet matches an image against `matchImages` by host, port and an
  optional path prefix, and the ports must be equal strings
  (`pkg/credentialprovider/keyring.go` `URLsMatch`, Kubernetes v1.34.0,
  lines 262-268). An image `harbor.example.com/p/app` has no port, so kubelet
  does not match it against an entry `harbor.example.com:443`.

## Decision

1. An empty `plugin.audience` means `harbor-bridge-<clusterName>`. The
   chart uses this one value everywhere the audience appears: the bridge's
   `BRIDGE_AUDIENCE`, the kubelet entry's `serviceAccountTokenAudience` and
   the audience RBAC (ADR-0017). The NOTES print it, because every
   HarborAccess must name it.
2. An empty `plugin.matchImages` means one entry: the host and port of
   `harbor.url`, without the scheme's default port (443 for https, 80 for
   http) and without a path.
3. A value that is set wins, as before. Existing installs set both values,
   because they were required, so an upgrade changes nothing.

## Consequences

- A first install needs four values instead of six.
- With the derived audience, a change of `clusterName` changes the audience.
  Every HarborAccess then reports `AudienceMismatch` until it names the new
  audience. docs/operations.md ("Changing clusterName") says to set
  `plugin.audience` to the old value to keep it.
- Releases on one cluster that share a `clusterName` (each on its own
  Harbor) get the same derived audience. They set `plugin.audience`, as the
  example in docs/operations.md ("Several installs per cluster") does.
- With the derived `matchImages`, a change of `harbor.url` moves the entry
  to the new host. A Harbor reached under another name than the one the
  pods pull from (an internal URL, a proxy) still needs `plugin.matchImages`.
- The check that refuses `matchImages` entries that match the registry of
  the chart's own images (ADR-0021) also applies to the derived entry: a
  cluster that pulls the bridge images from the same Harbor sets
  `plugin.allowSelfMatchImages` or a narrower `plugin.matchImages`.

## Alternatives considered

- **Keep both values required.** Rejected: both repeat other values in
  nearly every install, and each is a value a first install can get wrong.
- **Default the audience to the release name.** Rejected: the README and
  values.yaml already advise the cluster name, a release name such as
  `harbor-bridge` is the same on every cluster, and the audience is meant to
  differ between clusters that use one Harbor.
- **Make `trustPolicy.audience` optional in HarborAccess.** Not part of this
  decision. The audience in each object decides which bridge serves it when
  several bridges see it (ADR-0026); without it, a second bridge with an
  empty selector would create robots for every object.
