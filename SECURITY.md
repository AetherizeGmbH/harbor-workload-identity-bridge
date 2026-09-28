# Security model

This document describes what the bridge defends against, what it does
not defend against, and the operator-side choices that affect both. Read
it before installing.

The system-level threat model (STRIDE per trust boundary, residual
risks, logged security events) is in
[docs/threat-models/harbor-workload-identity-bridge.md](docs/threat-models/harbor-workload-identity-bridge.md).

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

The bridge verifies every SA token's signature against the signing keys
of its own cluster's apiserver (the JWKS it fetches at startup and
refreshes, `bridge/dataplane/oidc.go`). Each cluster signs its tokens
with its own ServiceAccount key, so a token from cluster B's API server
fails signature verification at cluster A's bridge. Clusters that share
a signing key (for example cloned control-plane key material) are not
separated by this check.

The bridge also enforces the `iss` claim against the configured issuer
(`BRIDGE_OIDC_ISSUER`). That check separates only clusters whose issuers
differ: the chart default and the kubeadm/kind default,
`https://kubernetes.default.svc.cluster.local`, are the same string on
every such cluster.

The audience (`aud`) claim must carry the one audience the bridge
serves (`plugin.audience`, [ADR-0026](docs/adr/0026-audience-pinning-and-harboraccess-selector.md)),
and the matching `HarborAccess` must name exactly that audience. A CR
that names another audience gets `Ready=False, reason=AudienceMismatch`
and no credentials (a robot it already had is disabled, see *Credential
lifetime, rotation, and revocation*): otherwise a CR author could name, say, the
apiserver's default audience and make every automounted token of that
ServiceAccount, including tokens leaked to third-party webhooks,
redeemable for Harbor credentials. The convention is
`harbor-bridge-<clusterName>`, one audience per cluster, so a token
minted for one cluster's bridge is refused by every other cluster's
bridge on its audience too, even where the issuers happen to agree. The
chart requires `plugin.audience` but cannot tell whether it is unique
per cluster; that is up to you.

Several bridges on one cluster each serve only the HarborAccess objects
their `bridge.harborAccessSelector` matches. A bridge revokes its robot
for an object that stops matching, then releases its own finalizers
(the per-instance one, and the shared one if the object's status names a
robot of its `clusterName`, i.e. it served the object before it had a
selector). An object the bridge refuses (another audience, for example)
gets no finalizer from it, and no Harbor lookup; one an older bridge
finalized while refusing it loses the finalizer once no robot of it is
left ([ADR-0032](docs/adr/0032-finalizer-ownership.md)). Bridges that
share a Harbor must use different `clusterName` values (the robot
ownership prefix). On the nodes, each bridge's release has its own
kubelet provider entry and plugin binary
(`plugin.providerName`, [ADR-0029](docs/adr/0029-configurable-plugin-provider-name.md)).
Keep their `plugin.matchImages` disjoint: kubelet runs every matching
provider for a pull, so with overlapping patterns each bridge receives a
token for its own audience and logs the requested image, also for pulls
it does not serve.

### Long-lived and unbound tokens

The bridge validates tokens locally against the apiserver's signing keys
([ADR-0006](docs/adr/0006-oidc-validation-and-audience.md)), so it cannot
ask whether a token's pod still exists. It therefore limits which tokens
it accepts ([ADR-0028](docs/adr/0028-token-lifetime-cap-and-pod-binding.md)):

- **Lifetime cap.** A token whose lifetime (`exp - iat`) exceeds
  `bridge.tokenValidation.maxLifetime` (default `1h`), that has no `iat`,
  or whose `iat` lies more than five minutes in the future is refused.
  Kubelet's credential-provider tokens last exactly one hour. Without the
  cap, `kubectl create token <sa> --audience=<aud> --duration=8760h` by
  anyone allowed to create tokens for that ServiceAccount yielded a
  credential valid at the bridge for a year.
- **Pod binding.** A token without the `kubernetes.io` pod claim (pod
  name and UID) is refused; every kubelet token is bound to the pod that
  pulls. `bridge.tokenValidation.requirePodBinding: false` turns this
  off and weakens the bridge; it exists for hand-minted tokens in local
  development only.

Both are `401` with `reason=invalid_token` and the audit category
`excessive_lifetime` or `not_pod_bound`. Which pod a token names is not
used for authorization: the ServiceAccount is the identity
([ADR-0010](docs/adr/0010-service-account-ref-as-identity.md)).

The cap limits how long one token can be redeemed at the bridge, not how
long the credentials it buys stay valid. The bridge answers with the
robot's password, and Harbor accepts that password until its next
rotation, which the bridge schedules 24 hours and one minute after the
previous one (`PasswordRotationInterval` plus `RotationSafetyMargin` in
`bridge/controlplane/contract.go`), or until you delete the robot's
password Secret or the `HarborAccess` (see *Replay of cached credentials
after revocation*).

Residual risk: a stolen kubelet token stays redeemable at the bridge for
its remaining lifetime, at most one hour, after its pod or its
ServiceAccount is deleted (only the apiserver's TokenReview checks the
bound object), and a password redeemed in that hour works at Harbor
until the next rotation. Whoever may create tokens for a ServiceAccount
can still mint a pod-bound token for an existing pod of it. One such
token per rotation keeps them supplied with credentials, so sustained
abuse shows up as about one TokenRequest a day in the apiserver audit
log, not one an hour.

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

A `HarborAccess` is cleaned up via its finalizer
([ADR-0023](docs/adr/0023-level-triggered-robot-lifecycle.md)):

1. Every robot the HarborAccess owns is deleted in Harbor: its current
   robot, robots of an earlier `serviceAccountRef` and dash-named robots
   from 0.2.x. A robot that is already gone is fine.
2. The robot Secret is deleted.
3. The finalizer is removed and the CR finishes deletion.

Deletion waits for Harbor: when listing or deleting a robot fails, the
finalizer stays, the HarborAccess reports `Ready=False`,
`reason=DeletionBlocked`, and the next reconcile retries. A bridge that
crashes between the steps resumes on the next reconcile, because the
finalizer is still there. The data plane stops issuing credentials for
the HarborAccess as soon as it is marked for deletion.

The CR can be gone while a robot survives only when someone removes the
finalizer by hand, the escape hatch for a Harbor that is gone for good
(README, "Uninstalling"). The robot's password then stays valid until
the janitor's next successful sweep (every 5 minutes, and only while
Harbor is reachable); delete the robot in Harbor yourself to close that
window. With a selector, the janitor, not the finalizer, revokes the
robot of a CR that stops matching and then releases its finalizers
([ADR-0026](docs/adr/0026-audience-pinning-and-harboraccess-selector.md),
[ADR-0032](docs/adr/0032-finalizer-ownership.md)). If
`harbor.robotNamePrefix` does not match Harbor's `robot_name_prefix`,
the bridge cannot recognise its robots by name, so it refuses to act:
a listing with a name the configured prefix does not account for fails,
and a robot whose description names the `HarborAccess` but whose name
is outside the bridge's ownership prefix is left alone and holds the
deletion. Either way deletion waits (`reason=DeletionBlocked`) until the
prefix is corrected. An empty Harbor `robot_name_prefix` cannot be
matched: an empty `harbor.robotNamePrefix` selects `robot$`.

### Credential leakage via logs

The bridge emits one structured audit line per credential issuance
(see *Audit log shape* below). The line names the matched CR, the
robot username, and the requested image, but **never the robot
password**. The admin credentials are read from disk for every Harbor
call and logged only as the directory path, never the values
(see `Sanitized()` in [`bridge/controlplane/config.go`](bridge/controlplane/config.go)).
`BRIDGE_HARBOR_URL` and `BRIDGE_OIDC_ISSUER` refuse a `user:password@`
part at startup: the bridge never authenticated with it. Only
`BRIDGE_OIDC_JWKS_URL` may carry one (it is sent as Basic auth to the
JWKS endpoint). The chart refuses one in `bridge.oidcJWKSURL`, where it
would be stored in plain text in the Deployment and the Helm release: set
such a URL in `bridge.extraEnv` from a Secret instead. The startup log
shows URLs with the whole `user:password@` part redacted, user name
included, and configuration errors never repeat a URL's credentials. A
URL setting with an `@` after its host part is refused: a `/`, `?` or `#`
inside a password ends the host early (percent-encode them), and the
value would otherwise be accepted and logged with part of the password
in it.

## What the bridge does *not* defend against

### Compromised node

If a node is compromised, containerd's memory can be inspected and
the robot password for any image pulled on that node read out. This
is fundamental: the container runtime needs the credentials in clear
form to talk to the registry. The bridge bounds the blast radius
through 24h rotation but does not eliminate it.

Mitigations:

- Treat node compromise as the credential breach it is. Rotate the
  affected robot's password immediately by deleting its password Secret
  in the bridge namespace: `kubectl -n <bridge-namespace> delete secret
  robot-<ha-namespace>.<ha-name>`. The bridge watches these Secrets and
  rotates at once (a missing Secret always forces a rotation). To revoke
  the robot entirely, delete the `HarborAccess`. A spec edit or a
  generation bump does **not** rotate (ADR-0023).
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
- To rotate that credential, refresh it in Harbor and update the Secret
  named by `harbor.adminCredsSecret`. The bridge reads the mounted files
  on every Harbor call, so the new value takes effect without a restart
  once kubelet has updated the volume (usually within a minute or two);
  the bridge then logs `Harbor admin credentials changed on disk`. Until
  then Harbor calls fail with `401`, and reconciles retry.
- Run the bridge under a strict `PodSecurityContext`: non-root,
  read-only root FS, no privilege escalation, dropped capabilities.
- Lock down `secrets` access in the bridge namespace via RBAC to
  the bridge ServiceAccount only.

### Unauthorized HarborAccess authorship

The bridge grants Harbor permissions purely from `HarborAccess` contents.
The reconciler creates a system-level robot with exactly the
`spec.permissions` a CR requests, bound to exactly the
`spec.serviceAccountRef` it names, and the data plane serves that robot's
credentials to any token whose `sub`/`aud`/`iss` match. There is **no**
check that the CR's author is entitled to those Harbor projects or to that
ServiceAccount identity, and the bridge honours `HarborAccess` CRs in
**any** namespace (it watches and lists them cluster-wide). There is no
admission webhook.

Consequence: **`HarborAccess` is a cluster-privileged resource.** Whoever
can create or update one can grant any workload identity they can run a pod
as `pull`/`push` on any Harbor project — including overwriting base-image
tags other tenants pull (a supply-chain vector). This is *not* the same as
the cross-tenant *read* protection above: that stops a pod reading another
workload's Secret; it does nothing about a malicious author *granting*
themselves access in the first place.

In a default install this is contained, because only cluster-admins can
create the CR (the chart grants no tenant-facing access to
`harboraccesses`). It becomes exploitable the moment you delegate CR
authorship — exactly the self-service, "one CR per workload, lives in git"
model this project encourages.

Mitigations:

- Treat `create`/`update`/`patch` on `harboraccesses.harbor.aetherize.io`
  as a privileged grant. Restrict it to the platform team via RBAC; do not
  fold it into the default `edit`/`admin` roles.
- Never wire `HarborAccess` into a tenant-writable GitOps path without an
  authorization policy. Ship a ValidatingAdmissionPolicy (CEL) or webhook
  that constrains, per source namespace, which `permissions[*].project` and
  which `serviceAccountRef.namespace` a CR may reference.
- Keep CR authorship in one trusted namespace and RBAC-lock creation there
  (convention today; a future `BRIDGE_HARBORACCESS_NAMESPACE` confinement
  knob would let the bridge enforce it).
- Scope the bridge's own Harbor identity to a system robot limited to the
  projects you actually reference (see *Compromised bridge* above). Then
  even an over-broad CR cannot reach projects outside that set — Harbor
  rejects the robot create/update.

### Token theft from a legitimately-authorised workload

If an attacker compromises a workload that *legitimately* has access
to a `HarborAccess`, they get the same credentials the workload has.
The bridge cannot distinguish "the workload's process" from "a shell
spawned in the workload's container". Mitigations are SA-token-scope
choices the operator makes upstream: `automountServiceAccountToken:
false` when not needed, short-lived projected tokens, audience-scoped
tokens. The bridge accepts no token that lives longer than
`bridge.tokenValidation.maxLifetime` (see *Long-lived and unbound
tokens*), so a stolen token can be redeemed at the bridge for at most
that long. The robot password it was redeemed for keeps working at
Harbor until the next rotation, up to 24 hours; deleting the robot's
password Secret rotates it at once (see *Replay of cached credentials
after revocation*).

### Replay of cached credentials after revocation

Kubelet caches credentials per `cacheKeyType` returned by the plugin.
The bridge emits `Registry` ([ADR-0016](docs/adr/0016-credential-provider-cache-key-type.md)),
so kubelet keys its cache by `(SA, registry-host)` for the
`cacheDuration` driven by `spec.tokenTTL` (default 1h, max 24h) and
capped at the next scheduled rotation.

Consequences:

- After you delete a `HarborAccess`, kubelets on each node may still
  serve cached credentials for that SA until the entry expires.
- The scheduled 24h rotation never outlives a cache entry: the bridge
  caps every `cacheDuration` at the Secret's `rotation-not-before`
  instant and rotates only after it (see *Credential lifetime,
  rotation, and revocation*). A forced rotation does break cached
  entries: when the robot Secret is deleted or incomplete, holds another
  robot's credentials, or the robot was re-created in Harbor. Kubelets
  that cached the old password then fail their pulls until the entry
  expires (at most `tokenTTL`).
- Rotation is not how a revoked identity loses access: that happens when
  its robot is deleted (or, for a refused HarborAccess, disabled) in
  Harbor.

Mitigations:

- Use the shortest `tokenTTL` that still keeps your pull rate
  reasonable. The bridge is cheap to ask; cluster-local NodePort
  hop, no Harbor round-trip for already-issued robots.
- For *immediate* revocation, delete the robot's password Secret
  (`robot-<ha-namespace>.<ha-name>` in the bridge namespace): the bridge
  rotates the password at once, and every copy of the old one fails its
  next Harbor handshake. Kubelets that cached the old credentials fail
  their pulls until the cache entry expires (at most `tokenTTL`), so
  expect pull errors on the affected workloads for that long. Deleting
  the `HarborAccess` deletes the robot instead. A spec edit or a
  generation bump does not rotate (ADR-0023).

### Privilege of the install DaemonSet

The plugin DaemonSet is the system's most privileged workload. In
every `plugin.install.mode` except `none`
([ADR-0021](docs/adr/0021-node-installer-modes.md)) its install init
container:

- runs as root (`runAsUser: 0`) and `privileged: true`.
- runs with `hostPID: true` and mounts the host's `/` read-write at
  `/host`. The broad mount is required because `merge` mode writes
  into whatever paths the node's kubelet flags point at — those are
  discovered at runtime from `/proc/<kubelet>/cmdline` and cannot be
  narrowed at chart-render time.
- finds kubelet through `/proc`, and accepts only a process named
  `kubelet` whose parent is PID 1, whose cgroup it can read and that is
  not a pod cgroup, and that shares init's mount namespace (no pod
  does, whatever its cgroup path looks like under a cgroup namespace).
  When a flag repeats, the last value wins, as in kubelet.
  Any pod can name its process `kubelet` and fake a command line; before
  this check such a pod could steer where the privileged installer wrote
  a root-owned binary. With several candidates left, it refuses to guess.
  Discovered paths must be absolute and clean.
- restarts kubelet via `nsenter -t 1 … systemctl restart <unit>` — no
  shell; the unit name is validated against the systemd character set —
  but only when the effective credential-provider config content (or
  the kubelet flags) actually changed, tracked by a content hash in
  `/var/lib/harbor-bridge/installer-state.json` (one state file per
  install, ADR-0029). It then waits until the
  unit is stably active, one kubelet process up for the whole settle
  period (and, in patch mode, until the running kubelet carries the
  flags), before it records success. Otherwise it restores the files it
  replaced, restarts kubelet onto them, records the content as rejected so
  that no retry restarts the same kubelet onto it again, and fails the pod
  (ADR-0033). Running containers survive the restart (containerd owns them).
  Binary drops and CA/mTLS rotation never restart kubelet.
- opens every host file relative to its directory and accepts only a
  regular file: a symlink, FIFO or device in place of a file it reads
  or replaces (including the `.bak` backups) fails the install. The
  sync container and any pod with a hostPath volume on the same
  directory can write next to these files; before this check a
  planted symlink turned the next install into a root write, or a copy
  of any host file into a readable backup, anywhere on the node. In
  merge mode this also means the node's existing credential-provider
  config must be a regular file. It opens `plugin.hostConfigDir` itself
  one path component at a time and refuses a symlink in any of them: a
  release whose `plugin.hostConfigDir` sits inside another release's is
  in reach of that release's sync container, which could otherwise
  replace the inner directory with a symlink and send the installer's
  root writes (CA, mTLS key, config, backups, lock files) into a
  directory of its choice. The check cannot look through a pod's own
  mounts: kubelet resolves the hostPath of `plugin.hostConfigDir` when it
  starts the pod, so in `none` mode, and for every release's sync
  container, a symlink planted before the pod starts is followed. Keep
  every release's `plugin.hostConfigDir` disjoint from every other
  release's directories (the bullet on the CA and mTLS files below).
- in `patch` mode parse-merges the environment file the kubelet unit
  reads, and only `/etc/default/kubelet` or `/etc/sysconfig/kubelet`
  (ADR-0034), preserving operator-set `KUBELET_EXTRA_ARGS`; in `merge`
  mode it edits the node's existing `CredentialProviderConfig`,
  preserving foreign provider entries and unknown fields. Both files, and
  their `.bak` copies, keep the node's owner and group and at most its
  permission bits (never wider than `0644`): a `0600` config stays
  `0600`.
- edits only the provider entry named `plugin.providerName` when several
  installs share a node ([ADR-0029](docs/adr/0029-configurable-plugin-provider-name.md)),
  and does so under an exclusive `flock` (`/run/harbor-bridge-installer.lock`
  for the whole pass, and a `.lock` file next to the provider config), so
  concurrent installers neither lose each other's entries nor restart
  kubelet at the same time. The lock files follow the same file rules as
  above.
- keeps a record `<bin dir>/<providerName>.entry` (mode `0600`, not
  executable) next to the plugin binary: the canonical bytes of its
  entry, and the chart-owned config it writes into, if any. A pass adds
  its new entry to the record before it writes the binary and the entry,
  and drops the old one from the record only after the config holds the
  new one. The bin dir is out of reach of the sync containers (no sync
  container mounts it, and the chart and the installer refuse a
  `plugin.hostBinaryDir` in reach of `plugin.hostConfigDir`, and a state
  dir inside it). Keep `plugin.hostBinaryDir` for this chart's plugins
  only.
- keeps the chart-owned config
  (`<plugin.hostConfigDir>/credential-provider-config.yaml`) the chart's:
  in `patch` and `none` mode, and in `merge` mode when kubelet's config
  is a chart-owned config (this install's own, or one a record in
  kubelet's bin dir names; for example a release in `auto` mode whose
  directories differ from those of the `patch`-mode release that wired
  kubelet). The sync container of every release that uses that
  directory, and any pod with a hostPath on it, can write there, and
  kubelet runs every entry of the file after its next restart. Each pass
  rewrites the file to this install's entry plus the other installs'
  entries, and keeps another install's entry only when its executable
  binary is in kubelet's bin dir (`plugin.hostBinaryDir` in `patch` and
  `none` mode) and its record there holds exactly that entry. A planted
  entry, or another install's entry changed in the file, is not written
  back by any installer, as the whole file was replaced before ADR-0029.
  Any other kubelet config inside `plugin.hostConfigDir` is refused in
  `merge` mode, and so is `plugin.install.configFile` inside it.
  Residual: kubelet reads the file whenever it starts, and nothing reads
  it again after the installer's write. A writer of
  `plugin.hostConfigDir` that swaps its own version in between an
  installer's write and the kubelet restart that installer triggers
  seconds later (it can watch the directory with inotify), or before any
  other kubelet start (a node reboot), has kubelet run a changed or
  planted entry until the next kubelet start that reads a clean file;
  the installer reports success. Such an entry can carry another
  release's `matchImages` and audience and point at the writer's own
  endpoint, which then receives that release's pods' tokens, or ask for
  any other audience the node may request tokens for. With one release
  per node only that release's own sync container could do this, to its
  own pulls; with a shared `plugin.hostConfigDir` every release's sync
  container can do it to every release's pulls. A writer can also remove
  another install's entry (or change it so that the next pass drops it)
  until that install's next pass; and after a pass of another install
  died between its record and its config, it can switch that install's
  entry between the old and the new one until that install's next pass.
  `plugin.hostConfigDir` must never be kubelet's credential-provider
  config directory (kubelet 1.34+): kubelet loads every config file of
  that directory at every start, and no pass removes a file a writer of
  the directory adds. In `patch` mode the installer also refuses to
  point kubelet at its own directories while the config kubelet reads
  holds another install's entry; in a config in reach of a
  `plugin.hostConfigDir`, only an entry a record in kubelet's bin dir
  vouches for counts, so a planted entry cannot keep kubelet on that
  config. It moves kubelet anyway when the config it moves to keeps that
  install's entry, which takes that install's record in the new bin dir
  (ADR-0035): a writer of `plugin.hostConfigDir` cannot bring that about,
  since only root on the node writes the bin dir.
- in `merge` mode into a cloud's config, keeps every other entry, but
  refuses to write the config, or restart kubelet onto it, when kubelet
  would exit at startup: an entry whose binary is missing from kubelet's
  bin dir or not executable, a name that occurs twice, or a name kubelet
  refuses.
- puts each release's CA file (which its plugin reads on every exec)
  and mTLS client certificate and key into `plugin.hostConfigDir`, which
  all releases share in `patch` and `none` mode, and in every mode with
  the default value. Every release's sync container mounts that
  directory read-write as root. Residual: a compromised sync container
  of one release, or any pod with a hostPath on the directory, can read
  the other releases' mTLS client keys and replace their pinned CA; each
  release's sync container restores its own CA only once per sync
  interval (60 s). A replaced CA breaks that release's pulls, or, with a
  position on the path to that bridge's endpoint, lets the writer
  impersonate the bridge; a client key only passes the mTLS check, which
  never replaces the token (O2). Give each release its own
  `plugin.hostConfigDir` in `auto` or `merge` mode where that matters.
  Such per-release directories must be disjoint: never the same as,
  inside, or containing another release's `plugin.hostConfigDir`,
  `plugin.hostBinaryDir`, `plugin.install.binDir` or
  `plugin.install.stateDir`, or kubelet's credential-provider bin dir.
  A directory inside another release's `plugin.hostConfigDir` (such as
  `/etc/kubernetes/credential-provider-config/eu` under the default) is
  in reach of that release's sync container, which reads the CA and mTLS
  key there and can replace the directory with a symlink that kubelet
  follows when it mounts it into this release's pods. The chart and the
  installer compare only one release's own directories.
- treats the provider name, a chart value, as a file name in kubelet's
  bin dir, where GKE keeps kubelet itself: in a cloud's config (`merge`
  mode) it never replaces a provider entry of that name that is not a
  bridge entry, and, for a non-default name, it never replaces a file of
  that name that is not this plugin: only its own record next to the
  file, or the file already holding this plugin's bytes, makes it this
  install's. A pass that refuses changes no config, binary, record, CA,
  mTLS or state file; only the lock files it took, and their
  directories, may be new (next to the cloud's config in `merge` mode,
  next to kubelet's current config in `patch` mode).

This privilege model is what installing a credential provider on
nodes requires — managed node images (EKS, GKE, AKS) pre-wire their
own providers exactly this way, just at image-build time. Bottlerocket
exposes no writable host filesystem and is unsupported.

Operator choices:

- `plugin.namespace` runs the DaemonSet in a namespace of its own
  ([ADR-0027](docs/adr/0027-optional-plugin-namespace.md)), created with
  Pod Security `enforce: privileged`. The bridge namespace can then
  enforce `restricted`, and write access to it no longer means root on
  every node. The bridge CA reaches the plugin namespace through a
  trust-manager `Bundle` (required in this mode); the bridge gets no
  access to the plugin namespace. Without it, the release namespace must
  allow privileged pods, and **write access to it (create pods, edit the
  plugin ConfigMap or the TLS Secret) is equivalent to root on every
  node**: restrict it like cluster-admin.
- `plugin.install.mode: none` is the least-privilege configuration:
  files only, no `hostPID`, no privileged container, and only the two
  narrow `plugin.hostBinaryDir`/`hostConfigDir` hostPath mounts. The
  operator owns the kubelet flags.
- The Helm release is the install boundary. Anyone who can
  `helm upgrade` this chart can swap the plugin binary that kubelet
  on every node will exec next pull. Restrict the helm caller's
  RBAC accordingly.
- The DaemonSet's long-running container is the installer in `--sync`
  mode: root (it refreshes the on-host CA/mTLS files when
  cert-manager rotates them) but never privileged, no capabilities,
  `seccompProfile: RuntimeDefault`, read-only root filesystem, and a
  single hostPath mount: `plugin.hostConfigDir`, which also holds the CA
  and mTLS files of every other release that uses the same directory
  (see above). It never sees the host root, never uses nsenter, never
  touches kubelet.
- The DaemonSet pods get no ServiceAccount token (they never call the
  Kubernetes API).
- `plugin.enabled: false` removes the DaemonSet entirely, for nodes
  that get the plugin out of band ([docs/install-external-plugin.md](docs/install-external-plugin.md)).

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

## Credential lifetime, rotation, and revocation

([ADR-0023](docs/adr/0023-level-triggered-robot-lifecycle.md))

- **Rotation.** Each robot password rotates every 24h. The bridge stamps
  the robot Secret with the instant before which it will not rotate
  (`harbor.aetherize.io/rotation-not-before`) and never lets kubelet
  cache the credentials past it, so a rotation invalidates no cached
  credentials. A permission change does not rotate: Harbor applies it to
  the existing robot.
- **Permission changes reach Harbor and stay there.** Every reconcile
  compares the robot's grants in Harbor with the HarborAccess spec and
  rewrites them on any difference, including changes made in the Harbor
  UI. A robot an administrator disabled stays disabled and is reported
  as `Ready=False, reason=RobotDisabled`.
- **Revocation.** Deleting a HarborAccess deletes every robot it owns
  (also robots of an earlier `serviceAccountRef` and dash-named robots
  from 0.2.x) and its Secret before the finalizer is released. Changing
  `serviceAccountRef` deletes the previous identity's robot as soon as
  the new one is in place; until then the new ServiceAccount gets `503`
  (`reason=secret_for_previous_identity`), never the old robot's
  password. The janitor sweeps for anything left behind
  every 5 minutes. While Harbor is unreachable, the finalizer holds and
  the HarborAccess reports `reason=DeletionBlocked`.
- **Refused HarborAccess.** A HarborAccess reported as
  `AudienceMismatch`, `IssuerMismatch` or `InvalidSpec` gets no usable
  robot ([ADR-0030](docs/adr/0030-refused-harboraccess-suspends-its-robot.md)).
  The bridge deletes its Secret (even while Harbor is unreachable). If it
  already had a robot, the bridge disables it in Harbor (description
  token `suspended=true`), or deletes it when its grants cannot be written
  back (a pre-0.5.5 `*`), so a password handed out earlier stops working
  at once. Fixing the HarborAccess gives the robot a new password while it
  is still disabled, then re-enables it. A wrong `plugin.audience`
  therefore suspends every robot until it is corrected. A robot an
  administrator disabled is left disabled.
- **Residual window.** The bridge stops issuing credentials for a
  HarborAccess the moment it is marked for deletion (`credential denied`,
  `reason=harboraccess_deleting`), even while the deletion is blocked.
  kubelet can keep cached credentials of a revoked identity for up to the
  CR's `tokenTTL`, but they stop working the moment the robot is deleted
  in Harbor. They keep working only while the deletion is blocked (the
  Harbor API unreachable, or refusing the bridge's admin credential) —
  keep `tokenTTL` short and alert on `reason=DeletionBlocked`.

## Hardening the bridge

| Lever | Default | Recommendation |
| --- | --- | --- |
| `BRIDGE_HARBOR_ADMIN_DIR` credentials | shared `admin` | Provision a per-bridge Harbor **system robot** instead: system permissions `robot` create/read/update/delete/list, plus `repository` pull and push on the projects it may grant (Harbor lets a robot create only robots whose permissions are a subset of its own) |
| Harbor transport (`harbor.url`) | https required, no redirects followed | Plain http needs `harbor.allowInsecureHTTP: true`: the admin credentials travel on every call and robot passwords in responses. For a private CA set `harbor.caSecret` instead of falling back to http. Point `harbor.url` at the address Harbor's API answers on directly: the bridge refuses redirects, which would re-send the admin credentials to a target on the same host (in clear text if it is http://) |
| OIDC transport (`bridge.oidcIssuer` for discovery, `bridge.oidcJWKSURL`, the discovered `jwks_uri`) | https required | Plain http is refused at startup except to a loopback host (`127.0.0.1`, `::1`, `localhost`, i.e. `kubectl proxy` in local development): the signing keys decide every credential, and anyone on a plain-http path could substitute them |
| TLS between plugin and bridge | required (HTTPS) | Add mTLS via `BRIDGE_TLS_CLIENT_CA_FILE`; each cluster's plugin authenticates with a client cert |
| `tokenTTL` | per-CR, 5m–24h, a Go duration (`30m`, `1h`; no days) | Use 1h or less unless you have a measured pull-rate problem |
| `bridge.tokenValidation` | `maxLifetime: 1h`, `requirePodBinding: true` | Keep both. A longer `maxLifetime` only admits longer-lived hand-minted tokens; a shorter one refuses kubelet's one-hour tokens unless your token issuer caps lifetimes lower. `requirePodBinding: false` is for local development only |
| `plugin.install.mode` | `auto` | `none` for the least privilege (no `hostPID`, no privileged container, two narrow hostPath mounts; you wire the kubelet flags). `plugin.enabled: false` for no node agent at all |
| `plugin.audienceRBAC.create` | `true` | Keep `true` unless you're providing a tighter binding via admission webhook; the chart's binding is audience-narrow but `system:nodes`-broad |
| Pod security (bridge) | hardened by default (`runAsNonRoot`, `runAsUser: 65532`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, drops `ALL` capabilities, `seccompProfile: RuntimeDefault`) | Keep the defaults; relax only if a sidecar genuinely requires it |
| Bridge namespace RBAC | unset | Restrict `secrets` get/list/watch to the bridge ServiceAccount only |
| Helm caller RBAC | unset | Restrict who can `helm upgrade` this chart — they can swap the binary kubelet runs on every node |
| Network exposure | NodePort `:31443` | Cluster-local only; firewall the NodePort to the cluster network. With Cilium kube-proxy replacement, socketLB intercepts host-netns `127.0.0.1:31443` from kubelet without exposing the port externally |
| `HarborAccess` authorship | any principal RBAC-granted `create harboraccesses` | **Cluster-privileged** — whoever authors a CR grants any project to any SA identity. Restrict to the platform team; gate any tenant-writable path behind an admission policy constraining projects / `serviceAccountRef` per namespace. See *Unauthorized HarborAccess authorship* above |
| Bridge & plugin image refs | mutable tag (chart `AppVersion`) | Pin by digest (`bridge.image.digest` / `plugin.image.digest`) and verify image signatures at admission — a re-pointed tag silently changes the binary kubelet exec's on every node |
| `/metrics` endpoint | plain HTTP on port 8080, pod network only (ClusterIP Service `<release>-metrics`), never on the NodePort | Restrict it with a NetworkPolicy to your Prometheus if the pod network is shared. The series are aggregate counts only — no secrets, subjects, robots, or images |
| `tls.enabled` | `true` (cert-manager) | `false` still serves TLS: it switches to an operator-provided Secret (`tls.existingSecret`). The bridge reloads a renewed certificate without a restart |
| `harbor.robotNamePrefix` | `robot$` | Match Harbor's `robot_name_prefix`. On a mismatch the bridge cannot recognise its robots and stops with an error that points at the prefix: every HarborAccess reports `HarborError`, deletions wait (`DeletionBlocked`), and the janitor deletes none of the bridge's robots. A robot created under a mismatch is deleted again at once, and the data plane serves no robot Secret (`503`, `reason=secret_for_previous_identity`). An empty Harbor `robot_name_prefix` cannot be matched: an empty value selects `robot$` (open decision, README) |
| Go toolchain & dependencies | pinned in `go.mod` | Keep current — `go 1.26.0` is a security floor and the `toolchain` directive pins the patched release. The release images are built in a `golang` image pinned to that release, and the image build fails if its Go is older than the `toolchain` line (`hack/toolchaincheck`); Renovate bumps the two together. Renovate plus a CI `govulncheck` step keep reachable CVEs from regressing |

## Audit log shape

One structured line per decision on the `audit` logger. That logger is
fixed at info level: `BRIDGE_LOG_LEVEL=warn` or `error` does not silence
it.

```
credential issued
  source=10.0.3.17                       # TCP peer (the node, via the NodePort)
  client_cert=CN=harbor-bridge-plugin    # with mTLS; the CN is <fullname>-plugin
  subject=system:serviceaccount:flux-system:source-controller
  pod=source-controller-7d9f  pod_uid=3f2c…  node=node-a   # bound-token claims
  audience=harbor-bridge-prod-eu-west
  harboraccess=harbor-bridge-system/flux-access
  generation=3
  robot=robot$bridge-prod-eu-west.flux-system.source-controller
  ttl_seconds=3600
  requested_image=harbor.example.com/production/myimg:v1   # asserted by the caller, max 512 chars

credential denied
  source=…  reason=invalid_token|no_matching_harboraccess|invalid_harboraccess_spec|secret_owner_mismatch|harboraccess_deleting
  category=expired|bad_signature|wrong_issuer|malformed|excessive_lifetime|not_pod_bound|other   # invalid_token only
  (subject, pod, node, audiences once the token is valid) requested_image=…

credential unavailable                   # valid token, nothing issued: 503 or 500
  source=…  subject=…  pod=…  node=…
  reason=secret_missing|secret_for_previous_identity|secret_unreadable|harboraccess_lookup_failed|robot_name_unknown
  (harboraccess once matched, err for a 500)
  robot=…  expected_robot=…              # secret_for_previous_identity only
  requested_image=…

credential unavailable                   # token not judged: signing keys unavailable, 503
  source=…  reason=signing_keys_unavailable  err=…  requested_image=…
```

The pod and node come from the `kubernetes.io` claim of the token. The
bridge requires the pod claim to be present (ADR-0028); which pod and node
it names is recorded for attribution only, and no authorization decision
reads it. The token itself is never logged.

The credential endpoint accepts at most `bridge.rateLimit.perSource`
requests per second (burst `bridge.rateLimit.burst`) from one source IP
and answers `429` beyond that, before it parses the token. Request
headers are capped at 32 KiB, and TLS handshake errors are logged at
most 20 per second.

Greppable by any single field. The robot password is never logged,
and neither are the Harbor admin credentials: the Harbor client
switches off the SDK's wire dumps, which the go-openapi runtime would
otherwise enable whenever `DEBUG` or `SWAGGER_DEBUG` is set in the
bridge's environment (`TestNewClient_DebugEnvDoesNotDumpSecrets`).

Denials (token rejected, no matching CR, invalid HarborAccess spec, CR
being deleted, Secret owner mismatch) are the `credential denied` lines
above, on the same fixed-info audit logger. A request with a valid token
that gets no credentials for another reason is a
`credential unavailable` line: the robot Secret does not exist yet, or
it still holds the robot of the previous `serviceAccountRef` (`503`, the
plugin retries; the latter also on every pull while
`harbor.robotNamePrefix` does not match Harbor's `robot_name_prefix`),
or it is incomplete, the robot name cannot be derived, or the Kubernetes
API failed (`500`, also on the regular log with the full error). So is a
token the bridge could not judge because it could not fetch the signing
keys (`503`, see below). Requests refused before the token is checked
(rate limit, missing bearer, bad body) are counted in the metrics below
but not logged one by one.

Every Harbor API call is bounded (30s per call, TLS 1.2 minimum, a cap
on paginated listings), so a Harbor that accepts connections and never
answers makes reconciles fail with an error and a `Ready=False`
condition instead of blocking the controller. The Harbor client follows
no redirects: net/http would re-send the admin credentials, and on
307/308 the request body, to a redirect target on the same host even
over plain http. A redirect fails the call with an error that names the
target (`TestClient_RefusesRedirects`). Robot listings, which deletion
and the janitor rely on to find every robot, page by robot ID rather
than by offset, so a robot deleted by someone else during the walk
cannot hide another one (`TestClient_List_ConcurrentDeleteHidesNoRobot`).

OIDC discovery and JWKS fetches follow no redirects and are bounded
(30s for discovery, 10s for a JWKS fetch). The bridge's own
ServiceAccount token, which the apiserver requires for them, is sent
only over https to the in-cluster apiserver and to the jwks_uri its
discovery names: anywhere else it could be replayed against the
apiserver with the bridge's RBAC. The signing keys are fetched again at
most every 30s (counted from the end of the previous fetch), so forged
tokens with unknown key IDs cannot make the bridge poll the apiserver
once per request. A token signed by a key the bridge already holds never
waits for a fetch: keys older than 10 minutes are refreshed in the
background, and while the apiserver is slow or unreachable, or answers
with no public key, the last keys fetched stay in use, including a key
the issuer has since rotated out. The bridge fetches the keys once at
startup and exits if it cannot, so a wrong `bridge.oidcJWKSURL`, CA or
RBAC stops the rollout instead of denying every request. A token signed
by a key the bridge does not hold, while it cannot fetch the current
keys, is answered `503` and counted as `reason=keys_unavailable`, not as
an invalid token.

The bridge also exposes Prometheus metrics for SOC-style alerting:

- `bridge_credential_issuances_total{result=ok|unauthorized|forbidden|unavailable|bad_request|server_error|rate_limited}`
- `bridge_oidc_validation_failures_total{reason=expired|bad_signature|wrong_issuer|malformed|excessive_lifetime|not_pod_bound|keys_unavailable|other}`
- `bridge_harboraccess_lookup_failures_total`
- `bridge_robot_secret_missing_total`
- `bridge_credential_issuance_duration_seconds`

A non-zero rate on `result=unauthorized` or
`oidc_validation_failures_total{reason=wrong_issuer|excessive_lifetime|not_pod_bound}`
is worth a page: kubelet never sends such tokens, so someone is trying
tokens the bridge does not trust.

## Verifying release artifacts

Every release image and the Helm chart are signed keyless with Sigstore
by the release workflows, and carry a signed SLSA build provenance
attestation. The images also carry a signed CycloneDX SBOM. All of it is
bound to the artifact digest. Kubelet runs the plugin binary as root on
every node, so verify before you deploy, and pin by digest.

Tags can be re-pointed. Each release pushes `:<version>`; `:latest`
follows the highest released version, and rebuilding an older release
does not move it back.

```bash
IMAGE=ghcr.io/aetherizegmbh/harbor-workload-identity-bridge   # or ...-plugin
cosign verify "$IMAGE@sha256:<digest>" \
  --certificate-identity 'https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/.github/workflows/release-images.yml@refs/heads/main' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation --type cyclonedx "$IMAGE@sha256:<digest>" \
  --certificate-identity 'https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/.github/workflows/release-images.yml@refs/heads/main' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify "oci://$IMAGE@sha256:<digest>" --owner AetherizeGmbH
```

The chart is signed by `release-chart.yml` (same command with
`.github/workflows/release-chart.yml@refs/heads/main` as identity, on
`ghcr.io/aetherizegmbh/charts/harbor-workload-identity-bridge@sha256:<digest>`).
Charts and provenance attestations from releases before the one that
introduced them are not signed; the images are signed since 0.3.1.

The release workflows build from the release tag, not from `main`,
without a build cache, and generate the SBOM in a job without any
token.

## Reporting a vulnerability

Email security@aetherize.com with the issue and a reproduction. We
will acknowledge within 5 business days. Please do not file public
issues for security bugs.
