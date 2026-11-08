# Operating the bridge

Reference for running the chart beyond the [quickstart](../README.md#quickstart): how the
plugin DaemonSet wires kubelet, several installs on one cluster, bootstrap
ordering, a change of `clusterName`, and a complete uninstall. Supported node platforms are in
[platforms.md](platforms.md).

## Node install modes and upgrades (ADR-0021)

The DaemonSet runs the `harbor-bridge-installer` binary on every node.
`plugin.install.mode` selects how kubelet gets wired:

| Mode | What it does | When |
| --- | --- | --- |
| `auto` (default) | Reads the live kubelet command line: flags present → `merge`, absent → `patch`. Flags that this install's own last patch-mode pass set (its state file says so) → `patch`, which moves kubelet when `plugin.hostBinaryDir` or `plugin.hostConfigDir` changed. While other installs that have not moved yet share the old directories, it stays there and also puts its files into the new ones; kubelet moves in the pass of the last of them (ADR-0035) | Almost always the right choice |
| `merge` | Injects our provider entry into the node's **existing** `CredentialProviderConfig` (JSON or YAML — EKS/GKE/AKS formats both work; foreign providers and unknown fields round-trip untouched) and drops the binary into the existing bin dir. Kubelet flags untouched. | Managed nodes (EKS AL2023, GKE, AKS) |
| `patch` | Own dirs (`plugin.hostBinaryDir`/`hostConfigDir`) plus a parse-merge of the environment file the kubelet unit reads (`/etc/default/kubelet`, or `/etc/sysconfig/kubelet` with RPM packages; ADR-0034) — operator-set `KUBELET_EXTRA_ARGS` are preserved | Self-managed nodes: kind, kubeadm (systemd kubelet whose unit reads one of the two files) |
| `none` | Files only; you own the kubelet flags. No hostPID, no privileged container, no host-root mount | k3s/RKE2 (flags via their kubelet args), strict-privilege environments |
| `plugin.enabled=false` | No DaemonSet at all; NOTES print the provider entry to install | Talos (system extension), baked node images |

`plugin.install.action=remove` turns every pod of the DaemonSet into a
removal of this install from its node; `helm uninstall` does that through a
pre-delete hook (ADR-0039, see [Uninstalling](#uninstalling)).

Kubelet reads the credential-provider config **once at boot** (no hot
reload), but execs the plugin *binary* per pull. The installer therefore
restarts kubelet exactly when the effective config content (or the
kubelet flags) changed — tracked via a content hash in
`/var/lib/harbor-bridge/installer-state.json` on each node (one state
file per install, see below):

- `helm upgrade` changing `matchImages`/`audience` **converges without
  operator action** (the DaemonSet re-rolls, the installer detects the
  content change, kubelet restarts once per node; running containers
  are unaffected).
- No-op re-rolls, plugin binary updates, and CA/mTLS rotation never
  restart kubelet (the plugin re-reads the CA on every exec; the
  DaemonSet's long-running container syncs rotated certs to the node).
- A value kubelet rejects no longer keeps the node down until someone
  repairs it, but it still costs an outage. The installer refuses a
  provider entry kubelet would exit on (`matchImages`,
  `defaultCacheDuration`, the audience) before it writes anything. When
  kubelet still does not come back after the restart (or, in patch mode,
  runs without the flags), the installer waits out its verification
  timeout (90 s), restores the files it replaced, restarts kubelet onto
  them and fails the pod with the reason. Kubelet is down for about that
  time, longer than the node-monitor grace period (40 s, 50 s since
  Kubernetes 1.32): the node turns `NotReady` and its pods drop out of
  Service endpoints until kubelet runs again. The installer does not
  restart kubelet onto the same content again: roll out corrected values,
  change kubelet (an upgraded binary, or another command line or
  `--config` file, gets one more try by itself), or fix the node and
  delete the state file there to retry (ADR-0033).

## Several installs per cluster (ADR-0029)

A cluster can run several bridges, for example one per Harbor instance
([ADR-0026](adr/0026-audience-pinning-and-harboraccess-selector.md)).
Each is a Helm release with a release name and a namespace of its own
and its own plugin DaemonSet. Kubelet has one credential-provider config
and one bin dir, so the releases share them: on every node, each
release's installer adds and updates only the provider entry named by
its `plugin.providerName` and leaves every other entry alone: the other
releases' and the cloud's. Values for a second release
(`helm install harbor-bridge-eu … --namespace harbor-bridge-eu --create-namespace`)
next to a first one that keeps the default provider name:

```yaml
clusterName: prod-eu-west-2          # distinct when both bridges use the same Harbor
plugin:
  providerName: harbor-bridge-eu     # kubelet entry and binary name; unique per release
  audience: harbor-bridge-prod-eu-west-2
  matchImages: ["harbor-eu.example.com"]   # must not overlap the other release's
bridge:
  harborAccessSelector:              # give every release a selector, and label
    harbor.aetherize.io/bridge: harbor-eu  # each HarborAccess for its bridge
service:
  nodePort: 31444                    # the first release keeps 31443
```

- **A release name per release.** The release name names the chart's
  cluster-scoped objects (the RBAC for the kubelet audience and, with
  `plugin.namespace`, the trust-manager Bundle), the plugin's mTLS client
  CN and, by default, `bridge.instance`, which names the bridge's
  finalizer. Two releases with the same name, even in different
  namespaces, collide. `fullnameOverride` replaces the release name in
  the object names and the CN, but `bridge.instance` follows only the
  release name: whenever `fullnameOverride` is what tells two releases
  apart, set `bridge.instance` explicitly too.
- **A namespace per release.** Install each release into a namespace of
  its own. The bridge's leader-election Lease has a fixed name
  (`bridge.harbor.aetherize.io`) in the release namespace, so two bridges
  there elect one leader between them and the other bridge's control
  plane does not run (leader election is on with the default two
  replicas). Each bridge's janitor also lists the robot Secrets of its
  `clusterName` in its namespace and deletes those whose HarborAccess its
  selector does not match: two bridges with the same `clusterName` (on
  different Harbors) in one namespace delete each other's Secrets.
- **A plugin namespace per release, or one created outside Helm.** With
  `plugin.namespace`, give each release its own, or create a shared one
  yourself and set `plugin.createNamespace=false` on every release.
  Otherwise the first release owns the Namespace object, installing the
  next one fails on it, and uninstalling the first deletes it together
  with the other releases' plugin DaemonSets.
- **Selectors on every release.** A bridge without
  `bridge.harborAccessSelector` sees every HarborAccess and marks the
  other bridge's objects `AudienceMismatch`. Label each HarborAccess for
  its bridge and give each release a selector.
- **Node files.** The default name keeps `harbor-bridge-plugin`,
  `harbor-bridge-ca.crt` (`harbor-bridge-client.crt`/`.key` with mTLS)
  and `installer-state.json`. Any other name uses `<name>` in the bin
  dir, `<name>.ca.crt` (`<name>.client.crt`/`.key`) in
  `plugin.hostConfigDir` and `<name>.installer-state.json` in
  `plugin.install.stateDir`. Every install also keeps a record of its
  entry next to its binary, `<name>.entry` (`harbor-bridge-plugin.entry`
  for the default name, mode `0600`).
- **Upgrade first, chart and plugin image.** Every release on the
  cluster must run a chart version with ADR-0029 **and a plugin image
  with its installer** before you add the second. The installer runs from
  `plugin.image` (`plugin.image.tag`, which defaults to the chart's
  version, or `plugin.image.digest`): a release that pins an older tag or
  digest keeps the older installer after the chart upgrade. With the
  default provider name, an older installer in patch or none mode
  rewrites the shared config with only its own entry, takes no locks and
  writes no record, so the other installs drop its entry in turn. With a
  non-default `plugin.providerName`, an older installer stops before it
  touches the node: the install container fails with
  `read rendered credential-provider config: open /config/credential-provider-config.yaml: no such file or directory`,
  the pods stay in `Init:CrashLoopBackOff`, and nothing changes on the
  node until the release gets a plugin image with ADR-0029. Add the
  second release only once every existing release's plugin DaemonSet
  runs such an image on every node, that is, once
  `kubectl -n <plugin namespace> rollout status daemonset/<fullname>-plugin`
  has finished for each (`<fullname>` is the release name or
  `fullnameOverride`). From then on, never roll a release back to a chart
  or plugin image before ADR-0029 while another release's plugin is on
  the nodes. (A single install can roll back: the installer keeps its
  state file readable for older installers, so an unchanged config does
  not restart kubelet.)
- **Same directories.** In patch and none mode all releases must use the
  same `plugin.hostBinaryDir` and `plugin.hostConfigDir`. Patch mode
  refuses to point kubelet at other directories while the config kubelet
  reads holds another release's entry (in a chart-owned config, only an
  entry that release's record in kubelet's bin dir vouches for), unless
  that release has its binary and record in the new bin dir and its
  entry in the new config already. To change the directories of all
  releases, change them in each, in auto mode: each release's pass puts
  its files into the new directories and keeps kubelet on the old ones,
  and the last one moves kubelet (ADR-0035). In auto
  mode a later release merges into whatever config kubelet already runs,
  and its binary goes into kubelet's bin dir. When that config is another release's
  chart-owned config, it treats it as the chart's (see "The chart-owned
  config stays the chart's" below). Otherwise keep kubelet's config out
  of every `plugin.hostConfigDir`: the installer refuses a kubelet config
  inside its own `plugin.hostConfigDir` that is not the chart-owned one,
  and a `plugin.install.configFile` inside it. Never make
  `plugin.hostConfigDir` kubelet's config directory (Kubernetes 1.34+):
  kubelet loads every config file of it, which every sync container can
  add and no pass removes.
- **Shared CA and mTLS files.** Each release keeps its CA file and its
  mTLS client certificate and key in `plugin.hostConfigDir`, and every
  release's sync container mounts that directory read-write as root. Where
  releases share it (always in patch and none mode), a compromised sync
  container of one release can read the other releases' mTLS client keys
  and replace their pinned CA until their own sync container restores it
  (every 60 s). That breaks the other release's pulls, and, together with
  a position on the path to its bridge's endpoint, lets the attacker
  impersonate that bridge; the token check still applies. In auto or
  merge mode a `plugin.hostConfigDir` per release avoids this
  (SECURITY.md). Per-release directories must be disjoint: never the
  same as, inside, or containing another release's
  `plugin.hostConfigDir`, `plugin.hostBinaryDir`, `plugin.install.binDir`
  or `plugin.install.stateDir`, or kubelet's credential-provider bin dir.
  `/etc/kubernetes/credential-provider-config/eu` under the default
  directory, for example, is in reach of the default release's sync
  container, which can read the CA and mTLS key there and replace the
  directory with a symlink. The installer refuses a symlink in its own
  `plugin.hostConfigDir`'s path, but kubelet follows one when it mounts
  the directory into a `none`-mode install container and every sync
  container, and the chart and the installer compare only one release's
  own directories.
- **Disjoint `matchImages`.** Kubelet runs every provider whose
  `matchImages` match an image, pools their credentials and tries them
  in order until a pull succeeds (read in the kubelet source; see
  ADR-0029). Overlapping patterns still pull, but every pull calls each
  matching bridge, and a bridge without a HarborAccess for the pod
  answers 403 and logs a denial each time; its plugin then exits non-zero,
  so kubelet logs an error for that provider on every such pull
  (ADR-0037).
- **Concurrent rollouts are safe.** The installers serialise on
  `/run/harbor-bridge-installer.lock` and on a `.lock` file next to the
  provider config, and each restarts kubelet only for changes to its own
  entry.
- **The chart-owned config stays the chart's.** In patch and none mode
  each pass rewrites `<plugin.hostConfigDir>/credential-provider-config.yaml`
  to its own entry plus the other releases' entries, and drops anything
  else, as the single install always did. It keeps another release's
  entry only when that release's binary and its record, holding exactly
  that entry, are in kubelet's bin dir (`plugin.hostBinaryDir`), which
  the sync containers cannot write. A merge pass into such a config (its
  own, or one another release's record names) does the same. That keeps
  every installer from writing or keeping a planted or changed entry,
  not kubelet from reading one: kubelet reads the file whenever it
  starts, so a sync container that swaps its own version in before a
  kubelet start, including the restart an installer triggers right after
  its write, gets its entries run (SECURITY.md; open decision O6). Keep
  `plugin.hostBinaryDir` for this chart's plugins only; the chart
  refuses a `plugin.hostBinaryDir` or `plugin.install.stateDir` in reach
  of `plugin.hostConfigDir`.
- **The installer refuses names it does not own**: in a cloud's config
  (merge mode), a provider entry of that name that is not a bridge
  entry; for a non-default name, a file of that name in kubelet's bin
  dir that is not this plugin (GKE keeps kubelet itself there) and has
  no record of this install next to it. In a cloud's config it also
  refuses to write or restart kubelet onto a config kubelet would exit
  on: an entry whose binary is missing from kubelet's bin dir or not
  executable, a repeated name, or a name kubelet refuses. A refused pass
  changes no config, binary, record, CA, mTLS or state file; only lock
  files (see [Uninstalling](#uninstalling)) may be new.
- `helm uninstall` of one release removes only its entry and files from
  the nodes, and keeps the other releases' entries and the flags they
  need; renaming its provider removes the old name (see
  [Uninstalling](#uninstalling), ADR-0039).

## Bootstrap ordering

The plugin can only run after its own image was pulled — so the bridge
and plugin images must come from a registry that `plugin.matchImages`
does **not** cover (the public GHCR images satisfy this by default).
The chart fails at template time if an exact `matchImages` entry equals
the images' registry host; air-gapped mirrors that accept the bootstrap
ordering can override with `plugin.allowSelfMatchImages=true`. A bridge
outage only affects *new* pulls of matched images: running containers
need no registry credentials, and kubelet's credential cache (per
ServiceAccount and registry, for each HarborAccess's `spec.tokenTTL`,
shortened to the robot's next scheduled password rotation) serves new
pulls by the same ServiceAccount on that node until it expires.
`plugin.defaultCacheDuration` does not set it: kubelet uses that value
only for a plugin response without a cache duration, and the plugin
always sends one.

## Changing clusterName

A bridge owns only the robots whose names start with
`bridge-<clusterName>.` and whose Harbor description names the same cluster
([ADR-0009](adr/0009-multi-cluster-topology.md),
[ADR-0018](adr/0018-dot-delimited-naming.md)). After a change of
`clusterName`, the bridge treats the robots of the old name as the robots of
another cluster:

- On its next pass over each `HarborAccess`, it creates a robot with the new
  name and writes that robot's credentials into the existing robot Secret.
  The bridge passes over every object when it starts. Until the write, it
  answers credential requests for that `HarborAccess` with `503`
  (`secret_for_previous_identity` in the audit log), because the Secret
  still holds a robot of the old name. Credentials that kubelet has cached
  still serve pulls in that time.
- It never rotates, disables or deletes a robot of the old name. Each old
  robot keeps its last password, and Harbor accepts that password until
  someone deletes the robot.

Delete the old robots by hand:

1. Wait until every `HarborAccess` this bridge serves shows `Ready` `True` and a
   `Robot` that starts with `robot$bridge-<new clusterName>.`:
   `kubectl get harboraccess -n <bridge namespace>`.
2. In Harbor, delete every robot whose name starts with
   `robot$bridge-<old clusterName>.`. Do this only when no other bridge
   that uses the same Harbor runs with the old `clusterName`.

## Uninstalling

Delete your `HarborAccess` objects **before** `helm uninstall`. Each one
the bridge serves carries a finalizer that revokes its Harbor robot, and
only a running bridge can release it — after the bridge is gone, deleting
those objects (or their namespaces) waits forever. If that already
happened, reinstall the bridge, or remove its finalizer by hand and delete
the leftover `robot$bridge-<clusterName>.*` robots in Harbor. The
finalizer is `harbor.aetherize.io/robot`, or, for a bridge with
`bridge.harborAccessSelector`, `harbor.aetherize.io/robot-<instance>`
(`bridge.instance`, default the release name; ADR-0026); the chart's
NOTES print the right one. An object that existed before the selector
was set may carry both: then remove both. Helm keeps the CRD (`crds/`); delete
it yourself when you are done.

Changing `bridge.harborAccessSelector` or `bridge.instance` later: a bridge
that gains a selector releases the shared finalizer from the objects it
served, including those that stop matching. Removing the selector leaves
`harbor.aetherize.io/robot-<instance>` on existing objects; the bridge
releases it because the chart keeps setting `BRIDGE_INSTANCE` (since
0.12.3; older charts set it only together with a selector). A renamed
instance never releases the old `harbor.aetherize.io/robot-<old instance>`. An object an
older bridge refused or found in `RobotConflict` carries the shared
finalizer without having been served; if it stops matching a new
selector, the finalizer stays (upgrade first: the bridge releases it from
refused objects it still selects). Remove such finalizers by hand once the
objects are served under the new settings.

### The nodes (ADR-0039)

`helm uninstall` removes the plugin from the nodes before it deletes the
release. A pre-delete hook Job switches the plugin DaemonSet to
`plugin.install.action=remove`, and the installer on every node takes this
install's provider entry and files off the node:

- **patch** (kind, kubeadm): the entry leaves the chart-owned config. When
  it was the last entry, the two `--image-credential-provider-*` flags go
  from `KUBELET_EXTRA_ARGS` (every other argument stays; an assignment left
  empty stays as `KUBELET_EXTRA_ARGS=""`) and the chart-owned config is
  deleted. Other installs' entries and the flags they need stay.
- **merge** (EKS, GKE, AKS): the entry leaves the cloud's config; the
  cloud's providers and the flags stay.
- **none**: the entry leaves the chart-owned config only when another
  install's entry stays in it. The binary stays: the installer cannot see
  which config kubelet reads, and kubelet does not start while a
  configured provider's binary is missing. Remove the flags (or the entry)
  yourself, restart kubelet, then delete the binary and its record.

**Order of the uninstalls.** On nodes where one release wired kubelet in
patch mode and another release, with other directories, merged its entry
into that release's chart-owned config (auto mode finds kubelet wired and
merges), uninstall the merging release first. It cannot remove the kubelet
flags the patch-mode release set, so it could never leave once it is the
last entry. The patch-mode release therefore refuses to leave first; its
error names the release to uninstall first. It leaves first when another
patch-mode release of the same directories stays, because that release
removes the flags last. Releases that share the same directories in patch
mode can leave in any order.

A `helm uninstall` right after `helm install`, while an install pass was
still restarting kubelet, works too: when kubelet runs with flags that name
this release's own directories and no state file was saved yet, the
removal takes the flags as this release's. When the flags are in kubelet's
environment file but kubelet does not run them yet, the removal refuses;
restart kubelet on the node and run `helm uninstall` again. It also refuses
when the config kubelet reads is missing.

Where what kubelet reads changed, the installer restarts kubelet and
verifies that it comes back, as an install does; a restart that does not
verify is rolled back (ADR-0033). Only then do the binary, its record, the
CA and mTLS files and the state file go, each with its `.bak` copy. The
lock files stay (other installers may hold them; `/run` is cleared at
boot), and so do the `.bak` copies of the shared files: kubelet's config
and, in patch mode, `/etc/default/kubelet.bak` or
`/etc/sysconfig/kubelet.bak`. They hold the content before the removal,
with this install's entry and flags, so restoring one needs the plugin
binary, which is gone.

The hook removes at most `plugin.uninstall.maxUnavailable` nodes at once
(25% of them, rounded up, plus the nodes whose plugin pod is down already),
which takes about four kubelet restarts, and waits at most
`plugin.uninstall.timeoutSeconds` (270 s, below Helm's default
`--timeout` of 5 minutes). Helm prints the hook's log, one line per node.

When a node cannot remove the plugin, the hook fails as soon as that node
reports, and names it and the installer's error; at its deadline it lists
every node that did not report (a NotReady or unreachable node never
does). Helm then stops: the release stays installed, the bridge keeps
running, and the DaemonSet stays in the removal action, so the other nodes
keep removing. Then:

- Fix the node and run `helm uninstall` again. It continues where the last
  run stopped; nodes that are done have nothing left to remove.
- Or give up on that node: `helm uninstall --no-hooks` deletes the release
  without the hook, and you clean that node by hand (below). Delete the
  failed Job too if its namespace stays: `kubectl -n <plugin namespace>
  delete job <release>-plugin-uninstall`.
- Or keep the plugin: `helm upgrade` with the same values sets
  `plugin.install.action` back to `install`, and every node installs again.
  A release installed with Helm 4's server-side apply needs `helm upgrade
  --force-conflicts`, because the hook changed fields that Helm owns.

A removal that kubelet rejected is not tried again on the same kubelet
(ADR-0033); the hook's error says so and names the node.

`plugin.uninstall.enabled=false` renders no hook: `helm uninstall` then
leaves the nodes as they are. So does `helm uninstall --no-hooks`.

The hook switches the DaemonSet only when Helm's annotations on it
(`meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`) name the
release the hook belongs to; Helm sets them on every object of a release.
A chart rendered with `helm template` has none, so the hook logs that the
DaemonSet is not managed by this Helm release and exits without touching
the nodes. If you deploy with `helm template | kubectl apply`, Kustomize
`helmCharts` or kapp, set `plugin.uninstall.enabled=false` (or render with
`--no-hooks`): those tools apply the hook's Job, ServiceAccount, Role and
RoleBinding as ordinary objects, at install time, and the RBAC stays in
the cluster. Argo CD maps a Helm `pre-delete` hook to its PreDelete hook,
which runs only when the Application is deleted (Argo CD documentation,
"Helm" and "Sync Phases and Waves"); the hook then does nothing either,
because Argo CD renders with `helm template`. Set
`plugin.uninstall.enabled=false` there too.

With any of these tools, uninstall in two steps: set
`plugin.install.action=remove` (with Helm: `helm upgrade --reuse-values
--set plugin.install.action=remove`; otherwise in the values you render
with, and apply), wait for `kubectl -n <plugin namespace> rollout status
daemonset/<release>-plugin`, then uninstall or delete the objects. The same two steps clean the
nodes before `plugin.enabled=false`, which removes nothing by itself: the
plugin may then come from elsewhere under the same name.

Renaming `plugin.providerName` removes the old name from every node in the
pass that installs the new one, with one kubelet restart (decision 6 of
ADR-0039). The installer knows the old name from the record it wrote under
it, which names the release, so the release must have run a version with
ADR-0039 once before the rename; a rename in the same upgrade leaves the
old entry and files, which the next rename or the uninstall then removes.
The old binary goes only after the old entry left every config on the node
the installer knows, a cloud's config included; a cloud's config that would
be left without providers keeps the old entry, and the old binary and
record stay. In none mode the old binary, CA and mTLS files stay: kubelet
runs the old entry until someone restarts it.

### Cleaning a node by hand

Nodes that the hook did not reach (after `--no-hooks`, a node that was
unreachable, a node the DaemonSet no longer selects, a release from before
ADR-0039) keep the provider entry named `plugin.providerName` in kubelet's
credential-provider config, and the plugin binary with its record
(`<providerName>.entry`), the CA (and mTLS) files, the state file, the lock
files and the `.bak` copies. Kubelet keeps running the plugin for its
`matchImages`; with the bridge gone the plugin fails and adds no
credentials, so those pulls go ahead with whatever other credentials
apply (a later service on the same NodePort would also need a
certificate from the pinned CA to receive a token). To clean such a node:

1. Remove the entry from the config kubelet reads
   (`--image-credential-provider-config`; in patch and none mode
   `<plugin.hostConfigDir>/credential-provider-config.yaml`). Kubelet
   refuses a config without providers: if it was the last entry, also
   remove the two `--image-credential-provider-*` flags (patch mode:
   from `KUBELET_EXTRA_ARGS` in `/etc/default/kubelet`, or
   `/etc/sysconfig/kubelet` with RPM packages).
2. Restart kubelet (`systemctl restart kubelet`). Do this before you
   delete the binary: kubelet does not start while an entry's binary is
   missing.
3. Delete `<bin-dir>/<providerName>` and its record
   `<bin-dir>/<providerName>.entry`, the CA and mTLS files in
   `plugin.hostConfigDir` and the state file in `plugin.install.stateDir`
   (file names in [Several installs per cluster](#several-installs-per-cluster-adr-0029)),
   each with its `.bak` copy. The binary's `.bak` is an earlier plugin
   binary, executable, in kubelet's bin dir.
4. Once the last install is gone from the node, also delete the backups
   of the shared files (`<provider config>.bak`; in patch mode
   `/etc/default/kubelet.bak` or `/etc/sysconfig/kubelet.bak`) and the
   lock files:
   `/run/harbor-bridge-installer.lock` and a `<config>.lock` next to
   every provider config an installer edited or checked, also in a pass
   it refused: next to the cloud's config in merge mode, and in patch
   mode next to the chart-owned config and next to any config kubelet
   read before patch mode moved it.
