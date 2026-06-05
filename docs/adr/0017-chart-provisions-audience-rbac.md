# 17. The chart provisions audience-scoped RBAC for kubelet token requests

## Status

Accepted

## Context

Since Kubernetes v1.32, the `ServiceAccountNodeAudienceRestriction`
feature gate is on by default. When enabled, the apiserver enforces
that a node (any `system:nodes` identity) may only request
ServiceAccount tokens with audiences for which the node is *explicitly
authorized*. Without authorization, kubelet's `TokenRequest` call
fails with:

```
serviceaccounts "X" is forbidden:
audience "Y" not found in pod spec volume,
system:node:NODE is not authorized to request tokens for this audience
```

The kubelet image-credential-provider (KEP-4412) calls `TokenRequest`
with the audience configured in
`CredentialProviderConfig.providers[].tokenAttributes.serviceAccountTokenAudience`.
For this bridge, that audience is the same string the
`HarborAccess.spec.trustPolicy.audience` field declares — typically
something like `harbor-bridge-<clusterName>`.

There is no pod-spec-volume route to authorise the audience for our
case: the SA token is minted by kubelet behind the credential
provider's back, not via a projected volume in the pulling pod's
spec. The only viable authorisation is RBAC granting the
`request-serviceaccounts-token-audience` verb to `system:nodes` for
the audience name as the resource.

The verb / resource shape is unusual — `request-serviceaccounts-token-audience`
is a virtual verb, the "resource" is the audience string itself
(not a real API object), and the apiGroup is `""`. Documented in
KEP-4412's authorization section. Operators are unlikely to discover
this incantation on their own; the failure mode is "the credential
provider silently never produces credentials", indistinguishable from
the documented upstream silent-abort bug.

Two ways the chart could handle this:

**Option A — document it, let the operator provide.** Lower blast
radius (no automatic `system:nodes` binding from the chart), but
requires every operator to write the same 14 lines of RBAC, and the
project's "install the chart and it just works" UX breaks.

**Option B — ship the RBAC as part of the chart.** One ClusterRole +
ClusterRoleBinding templated from the same `plugin.audience` value
the credential-provider config already uses, so there's no audience
drift. Cost: the chart creates a binding to `system:nodes` for the
specific audience name — a coarse subject grant.

## Decision

The chart ships both the `ClusterRole` and the `ClusterRoleBinding`,
templated from `.Values.plugin.audience`. Files:

- `charts/harbor-bridge/templates/audience-rbac.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "harbor-bridge.fullname" . }}-audience-token-request
rules:
  - apiGroups: [""]
    verbs:     ["request-serviceaccounts-token-audience"]
    resources: [{{ .Values.plugin.audience | quote }}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "harbor-bridge.fullname" . }}-audience-token-request
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind:     ClusterRole
  name:     {{ include "harbor-bridge.fullname" . }}-audience-token-request
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind:     Group
    name:     system:nodes
```

The audience name is the entire authorisation scope of these grants —
`system:nodes` can request a token for **that audience and no other**.
Since the audience embeds the cluster name by convention
(`harbor-bridge-<clusterName>` per the chart's own example), the grant
is narrowly scoped per cluster even though the subject is the
catch-all `system:nodes` group.

## Consequences

- Operators install the chart and it works on any cluster with
  `ServiceAccountNodeAudienceRestriction` enabled (i.e. v1.32+ by
  default). No additional manual setup, no silent failure mode where
  the credential provider runs but produces nothing.
- Every `system:nodes` member — i.e. every kubelet in the cluster —
  is authorised to mint tokens with the bridge's audience. The
  authorisation is narrow on the *audience* axis (only the one the
  chart configures) but broad on the *subject* axis. Acceptable
  because:
  (a) every kubelet is by design the consumer of the credential
      provider config, and
  (b) the audience name being cluster-scoped means a `system:nodes`
      in cluster A cannot exchange a token for the audience configured
      in cluster B.
- If an operator runs multiple `harbor-bridge` releases in the same
  cluster with different `plugin.audience` values, each release gets
  its own ClusterRole/Binding pair (named after the release).
- The RBAC follows the chart's lifecycle: `helm uninstall` removes
  it, no orphan ClusterRoles. (`helm.sh/resource-policy: keep`
  *not* set — we want clean removal.)
- Operators who prefer to provide their own RBAC (e.g. binding only
  to specific nodes via a NodeSelector mechanism, or restricting via
  a custom admission webhook) can disable the chart's RBAC with a
  new `plugin.audienceRBAC.create: false` toggle — exposed for the
  rare case but defaulted to `true`.

## Alternatives considered

- **Disable `ServiceAccountNodeAudienceRestriction`.** Rejected —
  trades a 14-line RBAC blob for cluster-wide loss of a security
  feature that's on by default since v1.32. The whole point of the
  feature gate is to prevent any kubelet from minting arbitrary
  audiences; disabling it cluster-wide because we want one specific
  audience is the wrong axis.
- **Bind only to specific nodes via a Role+RoleBinding on Node
  resources.** Not possible with this verb — the resource is the
  audience string, not a Node. The only subject knob is "who can
  request audience X" — pod-spec-audience-in-volume or a binding to
  some k8s identity (Group, SA, User).
- **Pre-declare the audience in every pod's spec via projected SA
  volumes.** Rejected — would require every workload's owner to add a
  projected volume to their pod spec just so kubelet can pull images.
  Defeats the credential-provider-plugin design, which is meant to be
  workload-transparent.
- **Bind to a non-Group identity (e.g. one ServiceAccount per
  kubelet).** Not how kubelet authenticates. Kubelets present as
  `system:node:<nodename>`, which is in `system:nodes`. Granting the
  whole Group is the standard pattern (mirrors how `system:nodes`
  receives all other kubelet-required permissions via Node-bound
  rolebindings).
