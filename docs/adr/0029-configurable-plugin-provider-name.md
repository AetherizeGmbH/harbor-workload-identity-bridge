# 29. Configurable plugin provider name

## Status

Accepted, 2026-09-27. Refines ADR-0021 (node installer) and ADR-0026
(several bridges per cluster); neither is reversed.

## Context

- ADR-0026 lets one cluster run several bridges, but only one
  chart-installed plugin could exist per node: the provider entry, the
  plugin binary, the CA and mTLS client files and the installer's state
  file had fixed names, so a second release's installer replaced the
  first one's entry and files (threat model item O5).
- Kubelet has exactly one `--image-credential-provider-config` and one
  `--image-credential-provider-bin-dir` (ADR-0021) and runs
  `<bin-dir>/<provider name>` for each entry. Several installs therefore
  share the config file, the bin dir, `/etc/default/kubelet` (patch mode)
  and the kubelet unit. Only the entry, the binary's name and the files the
  entry points to can belong to one install. (Since Kubernetes 1.34 the
  config flag may also name a directory whose `*.json`/`*.yaml`/`*.yml`
  files kubelet reads together; see Alternatives.)
- Patch and none mode wrote the chart-rendered config verbatim, which drops
  every other entry. Merge mode already replaced or appended by name.
- The installers of two releases can run at the same time on one node (a
  new node, two `helm upgrade`s). Unsynchronised read-modify-write of the
  shared file loses an entry, and two kubelet restarts at once confuse each
  other's verification.
- The content hash that decides about kubelet restarts (ADR-0021) covered
  the whole config file. With a shared file, every change by one install
  would make every other install restart kubelet again on its next pod
  start.
- Kubelet behaviour this rests on, read in the Kubernetes source (master,
  2026-09-27):
  - `pkg/credentialprovider/plugin/config.go` rejects a provider name that
    contains `/` or a space, is `.` or `..`, or occurs twice, and a config
    without any provider. Since v1.34.0 (commit `be6807e6a5`) the config
    path may be a directory; a name that occurs in two of its files is
    rejected too.
  - `pkg/credentialprovider/plugin/plugin.go` resolves each provider to
    `exec.LookPath(<bin-dir>/<name>)` at startup and fails when the binary
    is missing; `pkg/kubelet/kuberuntime/kuberuntime_manager.go` then exits
    kubelet (`os.Exit(1)`). An entry whose binary is gone keeps kubelet
    from starting.
  - `pkg/credentialprovider/plugin/plugins.go`
    (`externalCredentialProviderKeyring.Lookup`) runs every provider whose
    `matchImages` match the image and pools their credentials in provider
    order (`pkg/credentialprovider/keyring.go`, `BasicDockerKeyring` keeps
    every credential of a registry key);
    `pkg/kubelet/kuberuntime/kuberuntime_image.go` tries them in turn until
    a pull succeeds. A provider that fails is logged and contributes
    nothing.

## Decision

1. **`plugin.providerName`** (default `harbor-bridge-plugin`) names the
   provider entry and the plugin binary. The chart requires a DNS label of
   at most 63 characters: a subset of what kubelet accepts, safe as a file
   name, and free of dots, which the file names in (3) rely on. The
   installer reads it from `PROVIDER_NAME`, which the chart renders only
   for a non-default name, validates it the same way, and refuses a
   rendered config that has no entry of that name. The value must be a
   string: values files and `--set` read an unquoted `yes` or `123` as a
   boolean or a number before any template runs, so the chart refuses
   such a value (quote it, or use `--set-string`) instead of rendering a
   name the operator did not write. A missing value renders the default:
   `helm upgrade --reuse-values` from an older chart renders with that
   chart's values, which lack the key. The chart also quotes a
   non-default name in the rendered entry, because the installer's YAML
   parser reads `yes` or `123` the same way.

   For a non-default name the chart publishes the rendered config under
   the ConfigMap key `credential-provider-config.v2.yaml` and mounts the
   ConfigMap at `/config-v2`; the installer reads
   `/config-v2/credential-provider-config.v2.yaml` when `PROVIDER_NAME` is
   set, and refuses when that file is missing, with an error that says
   the chart and the plugin image must both have this ADR. The installer
   runs from `plugin.image`, which a release may pin to an older version
   than the chart. An installer before this ADR ignores `PROVIDER_NAME`;
   had it read the config, in patch or none mode it would install the
   entry of the new name next to a binary named `harbor-bridge-plugin`,
   and kubelet does not start with an entry whose binary is missing. It reads only `/config/credential-provider-config.yaml`,
   so it fails at that read, before it writes anything on the node, and
   the pod's sync container never starts. The default name keeps the old
   key and path, so existing installs render the same manifests.
2. **The default name keeps every node path.** Binary
   `harbor-bridge-plugin`, `harbor-bridge-ca.crt`,
   `harbor-bridge-client.crt`/`.key`, `installer-state.json`. An existing
   install renders the same manifests after the upgrade and nothing moves
   on the node.
3. **Any other name derives the install's own files from the name:** binary
   `<name>` in the bin dir; `<name>.ca.crt`, `<name>.client.crt` and
   `<name>.client.key` in `plugin.hostConfigDir`;
   `<name>.installer-state.json` in `plugin.install.stateDir`. A name has no
   dot, so two names never share a file and no derived name equals one of
   the default name's files. (`<name>-ca.crt` would not be injective: the
   name `harbor-bridge` would own the default install's CA file.)
4. **Each installer owns exactly its own entry.**
   - Merge mode edits the cloud's config: it replaces or appends only the
     entry of this name, and every other entry and unknown field
     round-trips (`map[string]any`, ADR-0021). An existing entry of this
     name that is not a bridge entry (no `HARBOR_BRIDGE_ENDPOINT` env) is
     never replaced. Nor does it write, or restart kubelet onto, a config
     kubelet exits on: an entry without a string name, a name with `/` or
     a space, `.` or `..`, a name that occurs twice, or an entry other
     than its own whose binary is not an executable file (or a symlink,
     which kubelet follows on the node) in kubelet's bin dir. It refuses
     instead; it does not remove the cloud's or another install's
     entries. Kubelet's other schema checks depend on its version and are
     not repeated.
   - Merge mode can also meet a chart-owned config (next bullet but one)
     as kubelet's config: this install's own, when only kubelet's bin dir
     differs from this install's (auto mode) or merge mode is set
     explicitly next to a patch-mode release with the same directories,
     or another release's, when a release in auto mode has other
     directories than the patch-mode release that wired kubelet. That
     file is in a `plugin.hostConfigDir`, and the merge pass may restart
     kubelet onto it. Merge mode then writes it as patch and none mode do:
     its own entry plus the entries the records in kubelet's bin dir
     vouch for, nothing else. A config counts as chart-owned when it is
     this install's `<plugin.hostConfigDir>/credential-provider-config.yaml`
     or a record in kubelet's bin dir names it: every patch- and none-mode
     pass, and every merge pass into a chart-owned config, names that
     config in its record (`chartOwnedConfig`). Any other kubelet config
     inside this install's `plugin.hostConfigDir` is refused, and so are
     `plugin.install.configFile` (`INSTALL_MERGE_CONFIG_FILE`) inside it:
     the override names a cloud's config.
   - Every pass keeps a record `<bin-dir>/<name>.entry` next to the
     binary: a JSON object whose `entries` hold the canonical bytes of
     the entries (JSON with sorted keys) this install may have in the
     config, and whose `chartOwnedConfig` names the chart-owned config it
     writes into (empty for a cloud's config). The record and the config are two files that no pass can
     replace together, so a pass first adds the entry it is about to
     write to the entries the record holds, then writes the binary and
     the entry, and only then reduces the record to that entry. A pass
     that dies in between, after a helm upgrade changed its entry, leaves
     both the entry still in the config and the new one in its record,
     and the other installs keep whichever the config holds until this
     install's next pass. The name has a dot, so it is never a provider
     name; the file has mode `0600` (only the root installers read it)
     and no execute bit, so kubelet cannot run it even under a planted
     entry of that name. The bin dir is the one directory the installers
     write that no writer of `plugin.hostConfigDir` reaches (next bullet
     but one); the sync container never mounts it. The record ties
     another install's entry and this install's binary to content only an
     installer writes.
   - Patch and none mode keep the chart-owned config
     (`<plugin.hostConfigDir>/credential-provider-config.yaml`)
     authoritative. That directory is writable by the sync container of
     every release and by any pod with a hostPath on it, and kubelet runs
     every entry of the file after its next restart, which the installer
     triggers itself. Installers before this ADR replaced the file with the
     rendered config on every pass, which removed anything planted there.
     Each pass now writes the rendered config plus the entries of the other
     installs, in place: bridge entries with a valid provider name whose
     binary is an executable regular file in `plugin.hostBinaryDir` and
     whose record there holds exactly the entry's canonical bytes (another
     install adds its entry to its record and writes its binary before
     the entry). An entry that matches none of its record's entries, or
     has no record, is dropped, also when a binary
     of its name is there (an uninstalled release leaves its binary, and
     any program in the bin dir is executable). This install's entry is
     replaced whatever it holds; every other entry, a repeated name and
     unknown top-level fields are dropped. While no other install's entry
     is in the file, it is the rendered config byte for byte, as before. A
     chart-owned file that cannot be parsed is replaced, as before.
   - What the installer trusts must be out of reach of the writers of
     `plugin.hostConfigDir`: the chart and the installer refuse a
     `plugin.hostBinaryDir` or `plugin.install.binDir` that is that
     directory, inside it or contains it, and a `plugin.install.stateDir`
     that is that directory or inside it (per path segment); merge mode
     refuses a discovered kubelet bin dir in that relation.
     `plugin.hostBinaryDir` must hold only this chart's plugins.
   - For a non-default name, an existing `<bin-dir>/<name>` is replaced
     only when this install's record is next to it or the file already has
     exactly the bytes the installer would write. A bridge entry of that
     name in the config does not count: in patch and none mode that config
     is writable by pods other than installers. The record keeps the
     binary this install's own when its entry has left the config (patch
     mode moving `plugin.hostConfigDir`, a cloud rewriting its config).
     GKE keeps `kubelet` itself in its credential-provider bin dir; a name
     like `kubelet` is refused instead of overwriting it.
   - A pass that refuses (a foreign entry or file of this name, a config
     it cannot use, another install's entry in the way of patch mode)
     refuses before it changes any config, binary, record, CA, mTLS or
     state file: those writes come after every check. Only the lock files
     it has taken by then (decision 5) and their directories may be new:
     the node lock; in merge mode the lock next to kubelet's config, which
     is usually the cloud's; in patch mode the lock next to kubelet's
     current config and this install's own config lock; in none mode
     that own config lock.
5. **Locks.** Every read-modify-write of a shared file and every kubelet
   restart runs under an exclusive `flock(2)`:
   - the node lock `/run/harbor-bridge-installer.lock`, a fixed path that
     every install finds whatever its values, held by auto, merge and patch
     mode for the whole pass: discovery, merge, restart, verification,
     state;
   - the config lock `<provider config>.lock` next to the file it guards,
     taken in every mode before the config is read. none mode mounts only
     its two directories and takes only this lock.
   - in patch mode, the lock of the config kubelet reads now (`<file>.lock`;
     for a directory of config files, the lock a none-mode install whose
     `hostConfigDir` is that directory takes) and this install's own
     config lock, one lock when both are the same, all taken before the
     installer reads kubelet's config to decide whether moving kubelet is
     safe, and held until the pass ends. Otherwise a none-mode install
     could add its entry to that config after the check and before
     kubelet moves away from it; that includes the case where kubelet
     reads this install's own config and only the bin dir moves.

   The order is always node lock, then the current config's lock, then
   this install's config lock; none mode holds one lock at a time, so no
   two installers wait for each other in a cycle.

   Lock files are created and opened under the installer's file rules
   (`os.Root`, Lstat, `O_NOFOLLOW`, `os.SameFile`; `installer/files.go`). A
   flock dies with its process, so a crashed installer leaves no stale
   lock. A waiting installer gives up after ten minutes and fails its pod,
   which retries.
6. **Restart responsibility is per install.** The state file gets a new
   field `entryHash`, which covers this install's entry, and in patch mode
   `/etc/default/kubelet`, not the whole shared file; the installer
   compares it to decide about a restart. A kubelet restart is still due
   whenever the pass changed a file. `appliedHash` keeps its meaning from
   before this ADR, the hash of the whole effective file (and, in patch
   mode, `/etc/default/kubelet`) at this install's last verified restart,
   because older installers compare exactly that field: a rollback of a
   single, unchanged install to an older chart or plugin image finds its
   own hash and does not restart kubelet. A record without `entryHash`
   (written before this ADR, or by an older installer after a rollback)
   is compared by `appliedHash`; when that matches the file as it is now,
   the installer adds `entryHash` without restarting kubelet.
7. **Identities do not follow the name.** The mTLS client certificate keeps
   its CN `<fullname>-plugin` (the release name unless `fullnameOverride`
   is set), which already differs per release; the bridge only logs it
   (identity checks are open item O2). The plugin binary reads
   everything from its entry's env and needs no change.

## Consequences

- Several releases, each with its own bridge and its own chart-managed
  DaemonSet, coexist on a node. Per release they need a distinct
  `plugin.providerName`, release name, namespace, `service.nodePort`,
  `plugin.audience` and `bridge.harborAccessSelector` (and `clusterName`
  when they share a Harbor, ADR-0026), and, with `plugin.namespace`, a
  plugin namespace of their own or a shared one created outside Helm
  (`plugin.createNamespace=false` on every release; otherwise one release
  owns the Namespace object, and uninstalling it deletes the others'
  DaemonSets). The fullname (the release name, or `fullnameOverride`)
  names the cluster-scoped audience ClusterRole and ClusterRoleBinding
  (without the namespace), the trust-manager Bundle and the mTLS CN; two
  releases of the same fullname collide even in different namespaces.
  `bridge.instance`, which names the finalizer, defaults to the release
  name only, not to `fullnameOverride`: when `fullnameOverride` is what
  tells two releases apart, `bridge.instance` must be set explicitly.
- A namespace per release is required, not only advised: the bridge's
  leader-election Lease has a fixed name (`bridge.harbor.aetherize.io`,
  `bridge/cmd/main.go`) in the release namespace, so two bridges in one
  namespace elect one leader between them and the other's control plane
  does not run; and the janitor lists the robot Secrets of its
  `clusterName` in its namespace and deletes those whose HarborAccess its
  selector does not match (`bridge/controlplane/janitor.go`
  `sweepSecrets`), so two bridges with the same `clusterName` in one
  namespace delete each other's Secrets. A possible follow-up makes both
  per instance (a Lease name and a Secret label derived from
  `bridge.instance`), which would allow several releases per namespace.
- In patch and none mode all releases must use the same
  `plugin.hostBinaryDir` and `plugin.hostConfigDir`, because kubelet
  reads one config from one bin dir: patch mode reads kubelet's wiring first and
  refuses to move kubelet to its own directories while the config kubelet
  reads (a file, or the files of a directory) holds a bridge entry of
  another name; a single install that changed its own directories still
  moves. When that config is in reach of the writers of a
  `plugin.hostConfigDir` (inside this install's, or a chart-owned config a
  record in kubelet's bin dir names), only the entries the records there
  vouch for count, as in decision 4: a planted entry neither keeps kubelet
  on that config nor keeps this install off its own. In a cloud's or
  hand-edited config every bridge entry counts. In auto mode a later install merges into whatever config kubelet
  already runs; when that is another release's chart-owned config, it
  keeps only the entries the records vouch for (decision 4). Its binary
  and record then go into the other release's `plugin.hostBinaryDir`,
  kubelet's bin dir.
- `plugin.matchImages` of different installs should not overlap. They still
  work when they do, but every matching plugin runs for each pull: a bridge
  without a HarborAccess for the pod answers 403, its plugin returns no
  credentials and no cache entry, and that bridge logs a denial for every
  such pull.
- `helm uninstall` removes nothing on the nodes, as before: the entry, the
  binary, its record, the CA and mTLS files, the state file, the lock
  files and the `.bak` copies of every file the installer replaced (the
  binary's is an executable earlier plugin in kubelet's bin dir; also the
  shared config's and, in patch mode, `/etc/default/kubelet`'s) stay.
  Kubelet keeps running the orphaned plugin for its `matchImages`; the
  plugin cannot reach the removed bridge (a new service on the same port
  would also need a certificate from the pinned CA) and contributes no
  credentials. Cleanup is manual: remove the entry before, or together
  with, the binary, then restart kubelet; if it was the last entry, the
  kubelet flags must go too. Renaming `plugin.providerName`
  leaves the old entry behind in the same way. There is no small, safe
  automatic cleanup: DaemonSet pods also stop on every re-roll and drain,
  where removing the entry would be wrong.
- A second install may be added only once every existing install runs an
  installer with this ADR on every node: each release's plugin image, not
  only its chart, must be such a version, and
  `kubectl rollout status daemonset/<fullname>-plugin` (in its plugin
  namespace) must have finished. An older installer with the default
  name in patch or none mode rewrites the shared config with its own
  entry alone, takes no lock and writes no record (so the other installs
  drop its entry). For the same reason no install may be rolled back to
  a chart or plugin image before this ADR while another install is on the
  nodes. A single install can roll back without a kubelet restart
  (decision 6).
- Two installs with the same `plugin.providerName` still overwrite each
  other; the installer cannot tell them apart from an upgrade.
- A writer of `plugin.hostConfigDir` can no longer get a changed or
  planted entry kept: the next pass of any install drops it and, in patch
  mode, restarts kubelet onto the file without it. It can still remove
  another install's entry from the chart-owned config (delete it, change
  it so that the next pass drops it, or make the file unparsable), which
  lasts until that install's next pass, usually its next pod start: a
  denial of service against that install's pulls. After a pass of another
  install died between its record and its config, until that install's
  next pass, the writer can also switch that install's entry between the
  one in the file and the one the pass meant to write: both are entries
  that install's installer wrote. And kubelet reads the
  file as it is at its own start, before any installer runs: a node
  reboot makes whatever the file holds then live until the next pass, as
  before this ADR. Merge mode keeps the entries of a kubelet config that
  no record in kubelet's bin dir names as chart-owned; that holds for the
  chart-owned config of an installer before this ADR (which "Upgrade
  first" rules out next to another install) and for a none-mode config
  when kubelet runs binaries from another directory than that install's
  `plugin.hostBinaryDir`. There it refuses only a config kubelet would
  exit on. A kubelet config in another release's `plugin.hostConfigDir`
  that is not that release's chart-owned config is not recognised
  either: keep kubelet's config out of every `plugin.hostConfigDir`.
- Releases that use the same `plugin.hostConfigDir` (all of them in
  patch and none mode, and in every mode with the default value) share
  every release's CA file there, which its plugin reads on every exec,
  and mTLS client certificate and key (`0600`, owned by root). Every
  release's sync container mounts that directory read-write and runs as
  root, which needs no capability to read or replace root-owned files.
  A compromised sync container of one release, or any pod with a hostPath
  on the directory, can therefore read the other releases' mTLS client
  keys and replace their pinned CA; each release's sync container
  restores its own CA only at its next tick (60 s), so a writer that
  keeps replacing it holds it most of the time. A replaced CA makes that
  release's plugin fail its TLS handshake (a denial of service), or,
  together with a position on the path to that bridge's endpoint
  (`127.0.0.1` on the host network by default), lets the writer
  impersonate the bridge. A client key lets it pass that bridge's mTLS
  check, which is defence in depth only (every request still needs a
  valid token; O2). With one release per node a sync container reached
  only its own files. A directory per release for the CA and mTLS files,
  mounted only by that release's sync container, would close this and is
  a possible follow-up.
- Lock files stay on every node, also with a single install: in none mode
  the config lock next to the chart-owned config; in auto, merge and
  patch mode the node lock and the lock next to the config the pass
  edits; in patch mode also the lock next to kubelet's current config
  whenever that is another config (the first move away from a cloud's
  config, or from an earlier `plugin.hostConfigDir`). A refused pass can
  leave these lock files, and their directories, behind (decision 4).

## Alternatives considered

- **One config file per install in a config directory.** Kubelet 1.34+
  reads every file of a directory named by the config flag, which would
  make per-install files possible without editing a shared file. Moving
  an existing install from its file to a directory changes the kubelet
  flags (a restart on upgrade and new node paths), managed nodes point the
  flag at the cloud's own file that merge mode must edit anyway, and the
  installer would have to support both layouts. The installer still
  expects a file and refuses a flag that names a directory with an error
  that says so. Supporting it (a per-install file dropped into that
  directory) is a possible follow-up.
- **A subdirectory per install under `plugin.hostConfigDir`.** The sync
  container and any pod with a hostPath on that directory can write there;
  a symlink planted in place of the subdirectory would redirect the
  installer's root writes and need another layer of checks. Flat file
  names reuse the existing per-file rules.
- **Locking the shared file itself.** It is replaced by rename, so a lock
  on it would sit on an inode that is no longer the file.
- **Locking directories (`/run`, the config directory).** Works with
  flock, but couples the installer to anything else that locks those
  directories and hides the lock from operators.
- **Only the node lock.** none mode cannot reach it without a third
  hostPath mount, which would widen the least-privilege mode.
- **Removing the entry when the pod stops (preStop).** Runs on every
  re-roll and drain, not only on uninstall.
- **Always rendering `PROVIDER_NAME`.** Changes the pod template of every
  existing DaemonSet, re-rolling it on upgrade for no effect.
