# Security model

This document describes what the bridge defends against, what it does
not defend against, and the operator-side choices that affect both. Read
it before installing.

## Goals

A workload should be able to pull from Harbor with its existing Service
Account identity, with **stronger** guarantees than `imagePullSecrets`
on at least these axes:

1. The robot password is not stored in a Secret the workload can read.
2. Compromising one pod does not let an attacker enumerate or revoke
   credentials for other workloads.
3. Credentials rotate without manual operator action and without re-deploying
   any workload.
4. A misbehaving bridge in one cluster cannot manipulate robots that
   belong to another cluster sharing the same Harbor.

This is not a goal:

- The bridge does **not** prevent a compromised node from exfiltrating
  the robot password during a pull. Containerd holds it briefly in
  memory, exactly as it would with `imagePullSecrets`. Node compromise
  is out of scope for any credential-provider architecture.

## Trust boundaries

```
  Workload pod        Cluster's kubelet     Bridge          Harbor
  (untrusted)         (privileged)          (trusted)       (trusted)
  ───────────────►   ──────────────►       ───────►        ───────►
       │                    │                  │                 │
       │  pull image        │                  │                 │
       ┴                    │   exec plugin    │                 │
                            │ ───►             │  POST /v1/      │
                            │     plugin       │   credentials   │
                            │                  │ ───►            │
                            │                  │      Bearer:    │
                            │                  │      SA token   │
                            │                  │ ◄───            │
                            │                  │   robot creds   │
                            │  Basic Auth      │                 │
                            │ ──────────────────────►            │
                            │                                    │
                            │                  registry handshake│
                            │  ◄─────────────────── 401 + JWT    │
```

- The pod is untrusted. It does not see the SA token unless `automount`
  is on; it does not see the robot credentials at all.
- The kubelet is privileged. Compromising it is equivalent to
  compromising every pod on the node — credential providers cannot
  defend against that.
- The bridge is trusted by every cluster it serves. Compromising the
  bridge process gives an attacker the ability to mint robot
  credentials for any SA the operator has issued a `HarborAccess`
  for. This is the highest-value target in the system; see
  *Hardening the bridge* below.
- Harbor is trusted to honor robot ACLs.

## What the bridge defends against

### Cross-tenant credential reuse

The robot password lives in a `Secret` in the bridge's namespace, not
in the workload's namespace. A pod with default RBAC has no API
permission to read across namespaces. Even a pod with `secrets` read
RBAC in its own namespace cannot reach the robot Secret.

This is the most important difference from `imagePullSecrets`: with
`imagePullSecrets`, every pod with `kubectl exec` access (i.e. anyone
with `pods/exec` RBAC) can `cat` the Secret. Here, the workload SA must
also have `secrets get` cross-namespace, which is non-standard.

### Token forgery / replay across clusters

The bridge enforces the SA token's `iss` claim against the cluster's
own issuer (`BRIDGE_OIDC_ISSUER`). A token from cluster B's API server
cannot be replayed against cluster A's bridge because the issuer
strings disagree. The kubelet always projects tokens with the cluster's
own issuer; this can't be tricked.

The audience (`aud`) claim must match the `trustPolicy.audience` on the
matching `HarborAccess`. Operators choose this — convention is to use
the Harbor hostname (`harbor.example.com`).

### Cross-cluster robot manipulation

Bridges share a Harbor instance but never each other's robots:

- **Layer 1:** Each bridge only manages robots whose name starts with
  the dot-terminated ownership prefix `bridge-<cluster-name>.`
  ([ADR-0018](docs/adr/0018-dot-delimited-naming.md)).
- **Layer 2:** Each bridge only adopts a robot whose description
  contains `cluster=<cluster-name>`.

Because the cluster field is a dot-free DNS label, distinct cluster names
produce non-prefixing ownership prefixes: `bridge-prod.` is *not* a prefix of
`bridge-prod-eu.flux.svc` (the character after `bridge-prod` is `-`, not the
required `.`). So Layer 1 alone already isolates clusters, and there is **no**
"cluster names must not be hyphen-prefixes of each other" operator burden — the
earlier `-`-delimited scheme had one; ADR-0018 removed it. Layer 2 remains as
defense-in-depth. See [ADR-0018](docs/adr/0018-dot-delimited-naming.md) and
[ADR-0009](docs/adr/0009-multi-cluster-topology.md).

### Stale credentials after `HarborAccess` deletion

A `HarborAccess` is cleaned up via finalizer:

1. The Harbor robot is deleted (best-effort; missing robots are fine).
2. The robot Secret is deleted.
3. The finalizer is removed and the CR finishes deletion.

There is no path where the CR is gone but the robot persists. If the
bridge crashes between steps, the janitor catches the orphan robot
within one sweep interval (default 5 minutes).

### Credential leakage via logs

The bridge emits one structured audit line per credential issuance
(see *Audit log shape* below). The line names the matched CR, the
robot username, and the requested image, but **never the robot
password**. Admin credentials loaded at startup are read from disk
and logged only as the directory path, never the values
(see `Sanitized()` in [`bridge/controlplane/config.go`](bridge/controlplane/config.go)).

## What the bridge does *not* defend against

### Compromised node

If a node is compromised, containerd's memory can be inspected and
the robot password for any image pulled on that node read out. This
is fundamental: the container runtime needs the credentials in clear
form to talk to the registry. The bridge bounds the blast radius
through 24h rotation but does not eliminate it.

Mitigations:

- Treat node compromise as the credential breach it is. Rotate
  the affected robot immediately by deleting and re-creating its
  `HarborAccess` (or `kubectl patch` to bump generation, which forces
  a `RefreshSecret` call).
- Use Harbor project-level scopes aggressively. A robot with `pull`
  on only the projects a workload actually needs has a small blast
  radius even when exfiltrated.

### Compromised bridge

If the bridge process is compromised, an attacker can:

- Mint credentials for any SA that has a matching `HarborAccess`.
- Read and rotate any robot Secret in the bridge namespace.
- Issue Harbor admin operations through the configured admin
  credentials.

Mitigations:

- Run the bridge with a dedicated Harbor *system robot* (not the
  shared `admin` user). Limit it to robot-account management on
  the projects you actually reference. See [ADR-0009](docs/adr/0009-multi-cluster-topology.md)
  for the recommendation.
- Run the bridge under a strict `PodSecurityContext`: non-root,
  read-only root FS, no privilege escalation, dropped capabilities.
- Lock down `secrets` access in the bridge namespace via RBAC to
  the bridge ServiceAccount only.

### Token theft from a legitimately-authorised workload

If an attacker compromises a workload that *legitimately* has access
to a `HarborAccess`, they get the same credentials the workload has.
The bridge cannot distinguish "the workload's process" from "a shell
spawned in the workload's container". Mitigations are SA-token-scope
choices the operator makes upstream: `automountServiceAccountToken:
false` when not needed, short-lived projected tokens, audience-scoped
tokens.

### Replay of cached credentials after revocation

Kubelet caches credentials per `cacheKeyType` returned by the plugin.
The bridge emits `Registry` ([ADR-0016](docs/adr/0016-credential-provider-cache-key-type.md)),
so kubelet keys its cache by `(SA, registry-host)` for the
`cacheDuration` driven by `spec.tokenTTL` (default 1h, max 24h).

Consequences:

- After you delete a `HarborAccess`, kubelets on each node may still
  serve cached credentials for that SA until the entry expires.
- The bridge's 24h password rotation (`harbor.RefreshSecret`) means
  cached credentials *also* become invalid at Harbor within 24h
  regardless of `tokenTTL`. A cached entry that hasn't expired in
  the kubelet cache still fails the registry handshake once the
  underlying robot password rotates.

Mitigations:

- Use the shortest `tokenTTL` that still keeps your pull rate
  reasonable. The bridge is cheap to ask; cluster-local NodePort
  hop, no Harbor round-trip for already-issued robots.
- For *immediate* revocation, force a password rotation: bump the
  CR's `metadata.generation` via any non-spec change, which triggers
  the reconciler's `RefreshSecret` path. The cached credentials at
  every kubelet then fail their next Harbor handshake.

### Privilege of the install DaemonSet

The plugin DaemonSet is the system's most privileged workload. Its
install init container:

- runs as root (`runAsUser: 0`).
- bind-mounts the node's `/etc/kubernetes/credential-provider*`
  hostPaths so it can write the binary, config, and CA bundle.
- when `plugin.patchKubelet: true` (default), runs with
  `hostPID: true` and `nsenter`s into PID 1 to patch
  `/etc/default/kubelet` with `--image-credential-provider-{bin-dir,config}`
  flags and runs `systemctl restart kubelet` on the host; once per
  node, idempotency-guarded.

This privilege model is non-negotiable for installing a credential
provider on nodes the operator doesn't control the image of (kind,
kubeadm, k3s). Cloud-managed clusters (EKS, GKE, AKS) bake the
binary + config + kubelet flags into the node image instead.

Operator choices:

- `plugin.patchKubelet: false`: disables the nsenter + kubelet
  restart block; use when the node image already wires kubelet.
  The init container also drops `hostPID` in this mode.
- The Helm release is the install boundary. Anyone who can
  `helm upgrade` this chart can swap the plugin binary that kubelet
  on every node will exec next pull. Restrict the helm caller's
  RBAC accordingly.
- The DaemonSet's runtime container after install is a `sleep` loop
  with no special privileges. The init container only runs at pod
  start.

### Audience-scoped RBAC

With `ServiceAccountNodeAudienceRestriction` on (default since
Kubernetes v1.32), the apiserver enforces that kubelets can only
mint SA tokens for audiences they're explicitly authorised for. The
chart ships this authorisation via
[ADR-0017](docs/adr/0017-chart-provisions-audience-rbac.md):

```yaml
ClusterRole:        verbs: ["request-serviceaccounts-token-audience"]
                    resources: ["<plugin.audience>"]
ClusterRoleBinding: subjects: [Group: system:nodes]
```

Scope:

- **Narrow on the audience axis.** The grant covers exactly one
  audience: the value of `plugin.audience` configured for this
  release. Every other audience in the cluster is unaffected.
- **Broad on the subject axis.** The grant binds the whole
  `system:nodes` Group, so every kubelet in the cluster can mint
  tokens for this specific audience.

The combination is acceptable because every kubelet in the cluster
runs the same credential-provider config and would request the same
audience anyway. The convention `plugin.audience: harbor-bridge-<clusterName>`
makes the audience cluster-scoped, so a `system:nodes` member in
cluster A cannot exchange a token for the audience configured in
cluster B sharing the same Harbor.

Operators who want to bind to specific nodes via a custom admission
webhook can set `plugin.audienceRBAC.create: false` and provide
their own RBAC.

## Hardening the bridge

| Lever | Default | Recommendation |
| --- | --- | --- |
| `BRIDGE_HARBOR_ADMIN_DIR` credentials | shared `admin` | Provision a per-bridge Harbor system robot scoped to robot-account management |
| TLS between plugin and bridge | required (HTTPS) | Add mTLS via `BRIDGE_TLS_CLIENT_CA_FILE`; each cluster's plugin authenticates with a client cert |
| `tokenTTL` | per-CR, 5m–24h | Use 1h or less unless you have a measured pull-rate problem |
| `plugin.patchKubelet` | `true` | Set `false` on EKS / GKE / AKS / baked AMIs so the DaemonSet drops `hostPID` and the nsenter / kubelet-restart block |
| `plugin.audienceRBAC.create` | `true` | Keep `true` unless you're providing a tighter binding via admission webhook; the chart's binding is audience-narrow but `system:nodes`-broad |
| Pod security (bridge) | unset | `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, drop `ALL` capabilities |
| Bridge namespace RBAC | unset | Restrict `secrets` get/list/watch to the bridge ServiceAccount only |
| Helm caller RBAC | unset | Restrict who can `helm upgrade` this chart — they can swap the binary kubelet runs on every node |
| Network exposure | NodePort `:31443` | Cluster-local only; firewall the NodePort to the cluster network. With Cilium kube-proxy replacement, socketLB intercepts host-netns `127.0.0.1:31443` from kubelet without exposing the port externally |

## Audit log shape

One structured `Info`-level line per credential issuance, with these
fields (logr key=value):

```
credential issued
  subject=system:serviceaccount:flux-system:source-controller
  audience=harbor.example.com
  harboraccess=harbor-bridge-system/flux-access
  generation=3
  robot=robot$bridge-prod-eu-west.flux-system.source-controller
  ttl_seconds=3600
  image=harbor.example.com/production/myimg:v1
```

Greppable by any single field. The robot password is never logged.

Failures (token rejected, no matching CR, Secret missing) log at
`V(1)` with the same shape minus the fields that don't apply.

The bridge also exposes Prometheus metrics for SOC-style alerting:

- `bridge_credential_issuances_total{result=ok|unauthorized|forbidden|unavailable|bad_request|server_error}`
- `bridge_oidc_validation_failures_total{reason=expired|bad_signature|wrong_issuer|malformed|other}`
- `bridge_harboraccess_lookup_failures_total`
- `bridge_robot_secret_missing_total`
- `bridge_credential_issuance_duration_seconds`

A non-zero rate on `result=unauthorized` or
`oidc_validation_failures_total{reason=wrong_issuer}` is worth a page;
both indicate someone is trying tokens the bridge does not trust.

## Reporting a vulnerability

Email security@aetherize.com with the issue and a reproduction. We
will acknowledge within 5 business days. Please do not file public
issues for security bugs.
