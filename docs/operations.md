# Operating the bridge

Reference for running the chart beyond the [quickstart](../README.md#quickstart): how the
plugin DaemonSet wires kubelet, several installs on one cluster, bootstrap
ordering, and a complete uninstall. Supported node platforms are in
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
  answers 403 and logs a denial each time.
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
- Removing one release, or renaming its provider, leaves its entry on
  the nodes: see [Uninstalling](#uninstalling).

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
releases it only when `BRIDGE_INSTANCE` still names the instance, which
the chart sets only together with a selector. A renamed instance never
releases the old `harbor.aetherize.io/robot-<old instance>`. An object an
older bridge refused or found in `RobotConflict` carries the shared
finalizer without having been served; if it stops matching a new
selector, the finalizer stays (upgrade first: the bridge releases it from
refused objects it still selects). Remove such finalizers by hand once the
objects are served under the new settings.

`helm uninstall` removes nothing on the nodes. The provider entry named
`plugin.providerName` stays in kubelet's credential-provider config, and
the plugin binary with its record (`<providerName>.entry`), the CA (and
mTLS) files, the state file, the lock files and the `.bak` copies the installer keeps of every file it
replaced stay on disk. Kubelet keeps running the plugin for its
`matchImages`; with the bridge gone the plugin fails and adds no
credentials, so those pulls go ahead with whatever other credentials
apply (a later service on the same NodePort would also need a
certificate from the pinned CA to receive a token). Renaming
`plugin.providerName` leaves the old entry behind in the same way. To
clean a node:

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
