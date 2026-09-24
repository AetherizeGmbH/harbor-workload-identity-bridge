# 27. The plugin DaemonSet can run in its own namespace

## Status

Accepted. Extends ADR-0021 (node installer) and ADR-0024 (optional plugin).

## Context

- The plugin DaemonSet is privileged: in every install mode but `none` it
  runs with `hostPID`, as root, with the host root filesystem mounted.
  Even `none` uses hostPath volumes and root. It ran in the release
  namespace, next to the bridge, so that namespace could only enforce the
  Pod Security level `privileged`, although the bridge itself is
  `restricted`-compatible. Anyone who can create pods or write Secrets in
  that namespace becomes root on every node (audit 2026-09, H5).
- The plugin trusts the bridge through the bridge's CA, which lives in the
  bridge's TLS Secret. A pod cannot mount a Secret from another namespace.
- The bridge must not gain write access to other namespaces to hand the CA
  over.

## Decision

1. **Opt-in split.** `plugin.namespace` names the plugin's namespace. Empty
   (the default) keeps the plugin in the release namespace, as before.
2. **The chart creates the namespace** when `plugin.createNamespace` is true
   (the default), labelled `pod-security.kubernetes.io/enforce: privileged`
   (plus `audit`/`warn`). The operator then labels the bridge namespace
   `restricted`. With `createNamespace: false` the namespace must exist.
3. **trust-manager delivers the CA** in split mode. The chart creates a
   `Bundle` (trust.cert-manager.io/v1alpha1) whose source is
   `plugin.caBundle.source`, a Secret or ConfigMap in trust-manager's trust
   namespace (trust-manager reads sources only from there) that holds the CA
   signing the bridge's serving certificate, typically the CA Secret of the
   ClusterIssuer. Its target is a ConfigMap in the plugin namespace only
   (namespaceSelector on `kubernetes.io/metadata.name`). The DaemonSet mounts
   that ConfigMap instead of the TLS Secret. The bridge's permissions do not
   change.
4. **mTLS in split mode** issues the plugin's client certificate in the plugin
   namespace, which requires a `ClusterIssuer` for
   `bridge.mTLS.clientIssuerRef`; the chart refuses a namespaced `Issuer`.
5. The plugin ServiceAccount, ConfigMap, DaemonSet and client certificate
   move with it; the cluster-scoped audience RBAC is unaffected.

## Consequences

- With the split, the bridge namespace can enforce `restricted`, and write
  access to the bridge namespace no longer implies node root. Write access
  to the plugin namespace still does, as for any privileged node agent;
  restrict it to the cluster operators.
- The split requires trust-manager (and cert-manager). Without it the
  default layout is unchanged, and SECURITY.md documents that the release
  namespace is then equivalent to node root.
- Existing installs that set `plugin.namespace` move the DaemonSet: Helm
  deletes the old objects and creates new ones in the new namespace; the
  installer's state file on each node makes the move restart-free when the
  rendered configuration does not change.
