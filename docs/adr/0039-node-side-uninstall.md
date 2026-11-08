# 39. Node-side uninstall

## Status

Accepted, 2026-11-08. Refines ADR-0021 (node installer), ADR-0029
(several installs per node), ADR-0033 (rollback of a rejected restart),
ADR-0034 (the kubelet environment file) and ADR-0035 (staged moves), and
replaces the consequence of ADR-0029 that `helm uninstall` removes nothing
on the nodes (ADR-0029, lines 300-313). ADR-0029's rejection of a preStop
cleanup (line 437) stands.

## Context

### What an install leaves on a node

- `helm uninstall` removes nothing on the nodes (docs/operations.md:293-329).
  The open decision "Node-side uninstall" (docs/roadmap.md:153-161) names
  one option: a pre-delete hook that removes this release's entry and
  files on every node and restarts kubelet. It must have the installer's
  privileges, reach every node and leave other releases' entries alone.
- Every mode writes the CA and, with mTLS, the client certificate and key
  into `plugin.hostConfigDir` (`syncAuxFiles`, installer/run.go:1094-1129),
  and the record `<bin dir>/<name>.entry` and then the binary
  `<bin dir>/<name>` (`installPluginBinary`, run.go:1043-1058;
  record.go:79-115). The provider name derives every file name; the default
  name keeps the names from before ADR-0029 (names.go:51-70).
- **patch** also writes the chart-owned config
  `<plugin.hostConfigDir>/credential-provider-config.yaml` (run.go:29-31,
  287) and puts the two `--image-credential-provider-*` flags into
  `KUBELET_EXTRA_ARGS` of the environment file the kubelet unit reads
  (run.go:257-269, 296; envfile.go:43-84). `mergeExtraArgs` adds the flags
  to the existing assignment and rewrites it as one double-quoted line, or
  appends a new `KUBELET_EXTRA_ARGS="..."` line when the file has none
  (defaultkubelet.go:81-83, 93-95). RPM packages ship an empty
  `KUBELET_EXTRA_ARGS=` (ADR-0034, lines 19-20), so an assignment does not
  tell who added it. After a verified restart the pass saves a state file
  with the mode, the paths and the environment file (run.go:882;
  state.go:28-60).
- **merge** adds the entry to kubelet's existing config, which it never
  creates (run.go:719-727), and puts the binary and record into kubelet's
  bin dir (run.go:768-778). The flags stay the cloud's. It saves a state
  file as patch mode does.
- **none** writes the chart-owned config, the binary, the record and the
  CA and mTLS files, and no state file; it never touches kubelet
  (run.go:188-220). Its pod mounts only `plugin.hostBinaryDir` and
  `plugin.hostConfigDir` and has no hostPID (plugin-daemonset.yaml:74-80,
  145-159, 229-238). The operator points kubelet's flags at these files, or
  at a config of their own that names the binary (docs/platforms.md:13).
- **auto** resolves to patch or merge on each node (run.go:74-106) and
  records the resolved mode (state.go:30-32, 106-109). While other installs
  block a move, it stays on the old directories and stages its files in the
  new ones (`stayAndStage`, `stageOwnFiles`, run.go:446-505; ADR-0035).
- Every write that changes a file keeps the old content as `<file>.bak`
  (files.go:334-379), also for the record (record.go:86) and the state file
  (state.go:139). An interrupted write can leave `<file>.tmp-<16 hex>`
  (files.go:392-397). Lock files are never replaced or removed
  (lock.go:137-145).
- The sync container copies the CA and mTLS files again every 60 s
  (sync.go:19-38, main.go:148).

### Kubelet

- Kubelet exits at startup when the binary of a configured provider is
  missing, and refuses a config without providers (ADR-0029 Context,
  ADR-0033 Context, docs/operations.md:305-315). It reads the config only
  at startup and runs the binaries per pull (ADR-0021 Context). A
  provider that fails at a pull is logged and contributes nothing
  (ADR-0029 Context). So a binary deleted while kubelet runs only fails the
  pulls that provider matches; the next start reads the config again. The
  order of a removal is therefore fixed: the entry leaves the config
  kubelet reads, kubelet restarts, and only then the binary goes.
- Kubelet takes the last occurrence of a flag (discover.go:207-217).

### What the removal can reuse

The node lock and config locks and their order (lock.go:16-41); the record
check `siblingIn` (run.go:665-678) and `claimsChartOwned` (record.go:122-150);
`checkKubeletCanStart` (run.go:799-825); the restart policy, verification,
rollback and rejection record (`finishWithRestart`, `rejectRestart`,
`refuseRejected`, run.go:835-962); `removeRegularIn`, which removes only a
regular file and never follows a symlink (files.go:509-524).

### The DaemonSet

- It updates with `maxUnavailable: 10%` (plugin-daemonset.yaml:48-51). A
  percentage rounds up, and without surge the old pod stops before the new
  one starts (k8s.io/api v0.36.3, apps/v1/types.go:609-624). The default
  toleration `operator: Exists` keeps its pods on NotReady nodes
  (values.yaml:483-488). Its pods get no ServiceAccount token
  (plugin-daemonset.yaml:71-72; plugin-serviceaccount.yaml:9-14).
- `kubectl rollout status` counts a DaemonSet as rolled out when
  `observedGeneration` has reached `generation` and `updatedNumberScheduled`
  and `numberAvailable` have reached `desiredNumberScheduled` (k8s.io/kubectl
  v0.35.0, pkg/polymorphichelpers/rollout_status.go:107-115).
- A pod stops on every re-roll, eviction, preemption and node shutdown, not
  only on uninstall (ADR-0029, lines 311-313 and 437-438).

### Helm

Read in the Helm source, v3.20.0 and v4.2.3:

- `helm uninstall` runs the pre-delete hooks before it deletes any resource
  of the release. When a hook fails it returns before it stores the release
  as uninstalling, so the release stays deployed (v3
  pkg/action/uninstall.go:104-123; v4 pkg/action/uninstall.go:130-150).
- Hooks run in weight order (v4 pkg/action/hooks.go:74). Helm waits for a
  hook Job until it finishes, at most `--timeout`, 5 minutes by default
  (hooks.go:120; `helm uninstall --help`). It waits only for hook Jobs and
  Pods; any other kind counts as ready at once (v4
  pkg/kube/statuswait.go:78-90). A hook without a delete policy
  gets `before-hook-creation` (hooks.go:236-246). The annotation
  `helm.sh/hook-output-log-policy` makes Helm print the Job's logs
  (v4 hooks.go:249-265; v3 hooks.go:97). When a hook fails, the hooks before
  it that carry `hook-succeeded` are deleted (v4 hooks.go:141). `--no-hooks`
  skips every hook.
- Helm 3 upgrades with a three-way strategic merge of the old manifest, the
  new manifest and the live object (v3 pkg/kube/client.go:640-700): a live
  change to a field the chart renders goes back to the rendered value, and a
  field the chart does not render stays as it is. Helm 4 installs with
  server-side apply by default (`helm install --help`, Helm 4.3.0:
  `--server-side` default true) and does not force conflicts unless asked
  (v4 pkg/action/upgrade.go:90-92).

## Decision

1. **The installer gets a removal action.** `INSTALL_ACTION=remove` (the
   default is `install`) makes the install container run a removal pass
   instead of an install pass, and makes the sync container wait until it
   is stopped without copying any file. The removal pass removes this
   install's provider entry and this install's files from the node, and
   nothing that belongs to anyone else. It does not read the rendered
   config.

2. **Rules for every removal pass.**
   1. *Refuse before writing.* Every check runs before the first write, as
      in an install pass. A refused pass leaves the node as it found it,
      with kubelet running.
   2. *Locks.* The same locks as an install pass: the node lock for the
      whole pass (all modes but none), the config lock of each config
      before it reads it, and the lock of the config kubelet reads until the
      pass ends. It never removes a lock file.
   3. *Order.* First the pass changes what kubelet reads at its next
      start: its entry leaves kubelet's config, or, for the last patch
      install, the flags go (3.1). Then kubelet restarts and the restart is
      verified. Then the entry leaves the configs kubelet does not read
      (3.7), and then the binary, the record, the CA and mTLS files, the
      `.bak` and `.tmp-*` copies of these files go, and the state file
      last.
   4. *Never an empty config.* When dropping this install's entry would
      leave the config kubelet reads without providers, the flags that
      point kubelet at it must go in the same pass. The pass removes flags
      only in the case of 3.1. In every other case it refuses.
   5. *Which entries stay.* In a cloud's config every entry stays except
      this install's bridge entry. An entry of this name without
      `HARBOR_BRIDGE_ENDPOINT` is not this install's and stays, as merge
      mode never replaces it (providerconfig.go:196-198). A JSON file stays
      JSON, and a file that does not change keeps its bytes, as in
      `mergeProvider` (providerconfig.go:209-231). In a chart-owned config
      only the entries `siblingIn` vouches for stay, as every pass writes
      that file (ADR-0029, decision 4).
   6. *Only own files.* The pass deletes `<bin dir>/<name>` only when this
      install's record is next to it, the name is the default name, or the
      file has the bytes of the plugin in the image: the rule of
      `checkBinaryOwnership` (run.go:1069-1090). It never deletes
      `<kubelet's bin dir>/<name>` while kubelet's config holds an entry of
      this name that is not a bridge entry: kubelet runs that file for it.
      It removes regular files only (`removeRegularIn`). It deletes the
      binary before the record, so a binary is never left without the
      record that makes it this install's.
   7. *Restart, verify, roll back.* As in ADR-0033. The pass checks that
      kubelet can start with the new config (`checkKubeletCanStart`),
      writes it, restarts kubelet and waits until one kubelet process stays
      up for the settle period. It also checks the wiring of the running
      kubelet: unchanged, or after 3.1 no longer the removed one. When that
      fails it writes back the config and the environment file as it found
      them, restarts kubelet onto them, verifies again and records the
      removal content as rejected in the state file. The binary, record, CA
      and mTLS files are still there, so nothing else needs restoring. A
      later removal pass with the same content and the same kubelet
      refuses without a restart.
   8. *Crash safety.* After the verified restart the pass saves a state
      with mode `remove`, then deletes the files, the state file last. A
      pass after a crash between the write and the restart finds no such
      state and restarts kubelet, as in the crash window of ADR-0021; a
      pass after the verified restart does not. Every step checks what is
      on disk again, so a second pass converges, and a pass that finds
      nothing to remove writes nothing and restarts nothing.
   9. *Report.* The pass writes one JSON line to its termination message
      (`/dev/termination-log`): the provider name, the result (`removed`,
      `nothing`, `kept`), whether kubelet restarted, what it left on the
      node and why, or the error that failed it. The pod log says the same.

3. **What a removal changes, per mode.** The pass takes the mode from the
   state file and from the running kubelet's wiring (discovery), and the
   configured mode only to tell none mode apart.
   1. *patch, last install on its directories.* Kubelet's config holds no
      other entry that stays. The pass requires that its state records a
      verified patch-mode restart onto exactly the running wiring, that the
      unit still takes `KUBELET_EXTRA_ARGS` from the recorded environment
      file (`kubeletEnvFile`), and that this assignment holds exactly the
      two flags with these paths. It removes both flags and keeps every
      other argument and every other line. An assignment left empty stays
      as `KUBELET_EXTRA_ARGS=""`, which kubelet reads the same as no
      assignment. It restarts kubelet and verifies that it no longer runs
      with these flags; flags from another source may then take effect
      (kubelet takes the last occurrence, discover.go:207-217), which the
      verification accepts as long as kubelet stays up. Only then is the
      chart-owned config one kubelet does not read (3.7), and it goes with
      its `.bak`. A rollback therefore restores only the environment file.
   2. *patch, other installs remain.* It drops its entry, keeps the
      entries of the others, keeps the flags, restarts kubelet and
      verifies that the wiring did not change. The last install to leave
      removes the flags (3.1).
   3. *patch, flags not yet live.* When the environment file names other
      directories than the running kubelet, an install pass died between
      writing the flags and restarting kubelet. The pass refuses: the
      config kubelet reads at its next start is not the one it runs. When
      the file holds no flags any more while kubelet still runs with the
      ones the state records, an earlier removal pass of this install died
      before its restart; this pass restarts kubelet, as in 2.8.
   4. *merge into a cloud's config.* It drops its entry, leaves the flags
      to the cloud, restarts kubelet, verifies, and then deletes its binary
      and record from kubelet's bin dir. When its entry is the only one, it
      refuses: merge mode never creates that file, and the flags are not
      the installer's to remove.
   5. *merge into a chart-owned config* (a record names it, ADR-0029). As
      3.2. When its entry is the last one, it refuses unless 3.1 applies.
   6. *auto* resolves through the state file as above. Without a state
      file it handles the entries as in 3.2 and 3.4 and never removes
      flags.
   7. *Configs kubelet does not read.* This install's own chart-owned config
      when kubelet reads another one, the config of the directories the
      state file records, and a chart-owned config this install's record
      names: in each, under its lock, it drops its entry and deletes the
      file when no entry is left. These need no restart. This covers the
      files ADR-0035 staged and those left in earlier directories. It looks
      for its binary and record in kubelet's bin dir, in
      `plugin.hostBinaryDir` and in the bin dir of the state file.
   8. *none.* The pod cannot see kubelet's flags or restart kubelet. It
      takes the config lock of the chart-owned config. It drops its entry
      only when an entry of another install that `siblingIn` vouches for
      stays; otherwise it leaves the config as it is, because kubelet may
      read it and refuses it empty or missing. It keeps the binary: kubelet
      may run it from a config the installer cannot see, and exits at
      startup when it is missing. When it dropped the entry it reduces its
      record to no entries, which keeps the binary this install's for a
      later install and keeps every other install from vouching for a copy
      of the entry; otherwise the record stays as it is. It deletes the CA
      and mTLS files, which kubelet does not need to start. It reports
      `kept` with the manual steps and succeeds.
   9. *What stays on every node.* The lock files; the `.bak` copies of
      kubelet's config and of the environment file, which hold the content
      before the removal; the state directory and the two plugin
      directories.

4. **`plugin.install.action`** (`install` or `remove`, default `install`)
   renders `INSTALL_ACTION` into both containers of the DaemonSet. It is
   always rendered, so a `helm upgrade` sets it back to the chart's value
   after a hook changed it (Context, Helm). Setting it to `remove` and
   waiting for `kubectl rollout status` is the two-step uninstall for tools
   that do not run Helm hooks, and the way to clean the nodes before
   `plugin.enabled=false`. Turning the plugin off with `plugin.enabled`
   does not remove anything by itself: the plugin may then come from other
   means under the same name, which a removal would delete. The chart
   also renders `RELEASE_ID` (`<release namespace>/<release name>`), and
   every record stores it (decision 6), so that the pod template changes
   only once.

5. **`helm uninstall` switches the DaemonSet to removal and waits.** With
   `plugin.enabled` and `plugin.uninstall.enabled` (default true) the chart
   renders these pre-delete hooks in the plugin namespace:
   - A ServiceAccount, a Role and a RoleBinding, weight -5, delete policy
     `before-hook-creation,hook-succeeded`. The Role allows `get` and
     `patch` on the plugin DaemonSet by name and `list` on pods.
   - A Job, weight 0, delete policy `before-hook-creation,hook-succeeded`
     (a failed Job stays for `kubectl logs`), output log policy
     `hook-succeeded,hook-failed`. `backoffLimit: 0`, an
     `activeDeadlineSeconds` just above `plugin.uninstall.timeoutSeconds`,
     a security context that Pod Security "restricted" admits, and the
     bridge's node selector, tolerations and affinity, so that it does not
     land on a NotReady node through the plugin's `operator: Exists`. It
     runs `/installer/harbor-bridge-uninstall-hook` from `plugin.image`: a
     second binary in the plugin image, standard library only (ADR-0021).
     It sends the ServiceAccount token only to `KUBERNETES_SERVICE_HOST`,
     reads it again for every request, verifies the API server against the
     ServiceAccount CA and follows no redirect.

   The hook binary:
   1. reads the DaemonSet; when there is none it prints that nothing on the
      nodes is removed and exits 0. It also exits 0 without a change when
      the DaemonSet's Helm annotations (`meta.helm.sh/release-name`,
      `meta.helm.sh/release-namespace`, which Helm sets on every object of
      a release: helm.sh/helm pkg/action/validate.go:140-163, install.go
      and upgrade.go) do not name the release the hook was rendered for
      (`RELEASE_NAME`, `RELEASE_NAMESPACE`). A chart rendered with `helm
      template` has none: `kubectl apply`, Kustomize and kapp apply the
      hook like any other object at install time, and Argo CD runs a Helm
      `pre-delete` hook as its PreDelete hook when the Application is
      deleted (Argo CD documentation, "Helm"). Without this check the hook
      would remove the plugin from every node of a running install. Its
      first read and its patch are retried, like its polls, while the API
      server does not answer;
   2. patches it (strategic merge, its own field manager): `INSTALL_ACTION`
      `remove` in both containers, and `maxUnavailable` set to a number:
      `plugin.uninstall.maxUnavailable` (a number, or a percentage of
      `desiredNumberScheduled` rounded up; default `25%`) plus the
      DaemonSet's `numberUnavailable`, at most `desiredNumberScheduled`, so
      that pods that are already unavailable do not use up the budget;
   3. polls every 5 seconds until the rollout is done as `kubectl rollout
      status` defines it and every pod with the removal action has a
      report for this provider name, at least `desiredNumberScheduled` of
      them;
   4. then prints one line per node and exits 0, and Helm deletes the
      release;
   5. fails at once when a node reports a failed removal: it prints that
      node and its installer's error and exits 1. The other nodes keep
      removing, because the DaemonSet's rollout goes on without the Job;
   6. at `plugin.uninstall.timeoutSeconds` (default 270, below Helm's
      default timeout so that the Job, not Helm, reports) prints every node
      that has no report, with its pod's state, and exits 1.

   When the Job fails the release stays deployed, with the bridge running
   and the DaemonSet in removal mode, and running `helm uninstall` again
   continues. The hook never reverts its patch.

   With a `plugin.image` from before this ADR the hook binary is missing
   and the Job fails before it patches anything. A pod that runs such an
   installer (the DaemonSet's image differs from the Job's only when
   someone edits it) ignores `INSTALL_ACTION`, runs an install pass and
   writes no report, so the hook fails and names `plugin.image` instead of
   reporting success. Either way Helm deletes nothing.
   `helm uninstall --no-hooks` skips the hook and leaves the nodes as
   before this ADR.

6. **Renames remove the previous name.** An install pass of the name M
   for release R removes every other name P whose record in a bin dir the
   pass uses names R: in the same config write that adds M's entry
   (siblings named P are not kept, and P's entry leaves a cloud's config),
   with one kubelet restart, and P's files after the verified restart, as
   in decision 2. In none mode M's entry keeps the config non-empty, so P's
   entry goes, P's record is reduced to no entries and P's binary stays
   (3.8). P's binary goes only while kubelet's config holds no entry of
   that name: the pass may not have written the config kubelet reads (a
   mode change together with the rename). A removal pass also removes
   such names P, and counts P's state file for the flags of decision 3.1:
   the release set them under that name. A record without a release,
   written before this ADR, belongs to no release: a release must run this
   version once before a rename removes the old name. When two releases
   swap their names, each name's record names the release that wrote it
   last, so neither pass removes the other's entry. Earlier names do not
   block a patch-mode move (ADR-0035) either.

## Consequences

- One `helm uninstall` removes the entry and files from every node the
  DaemonSet runs on and leaves kubelet running. Each patch- or merge-mode
  node restarts kubelet once; none-mode nodes restart nothing. With the
  default pacing the rollout takes four waves, each about one kubelet
  restart, the 15 s settle period (restart.go:195) and a pod start: about
  three minutes, not measured.
- While the hook runs, nodes it has cleaned pull without the bridge's
  credentials while the bridge still serves the others, as after the
  uninstall.
- A node that cannot run the removal fails the uninstall: a NotReady or
  unreachable node, a refusal, a restart kubelet rejects. Helm prints the
  Job's log with that node. Running `helm uninstall` again continues where
  the last run stopped. `--no-hooks` gives up; the listed nodes then need
  the manual steps. To keep the plugin instead, `helm upgrade` with the same
  values sets the action back to `install` (Helm 3); under server-side
  apply, the default of Helm 4 for new installs, the hook's change makes
  Helm's apply conflict, and the upgrade needs `--force-conflicts`. Every
  node the hook had cleaned then restarts kubelet once more.
- The upgrade to the chart version with this ADR rolls the DaemonSet once
  (two new environment variables). The install passes find their content
  unchanged and restart no kubelet (run.go:842-846).
- After a removal the `.bak` of kubelet's config and of the environment
  file hold the content before the removal, with this install's entry and
  flags. Restoring them by hand needs the plugin binary, which is gone. An
  environment file whose assignment held only the two flags keeps
  `KUBELET_EXTRA_ARGS=""`.
- When one install stays on shared directories, the removal wrote the
  chart-owned config with the YAML encoder. That install's next pass writes
  its rendered config byte for byte (providerconfig.go:327-329) and
  restarts kubelet once more.
- In none mode the binary stays, and the entry too when it was the last
  one. The hook's output says so, and the operator removes the flags or
  the entry, restarts kubelet and deletes the binary and record.
- While the hook runs, a ServiceAccount in the plugin namespace may patch
  the plugin DaemonSet, whose pods are privileged: that is root on every
  node. Whoever can create pods or tokens in that namespace has that
  already (values.yaml:250-253). Helm deletes the RBAC after the hooks ran,
  also when the Job failed (hooks.go:141); an interrupted `helm` leaves it
  until the next attempt. SECURITY.md and the threat model say so.
  Trivy's misconfiguration scan of the rendered goldens at HIGH and
  CRITICAL finds nothing new in the Role or the Job.
- Tools that do not run Helm's pre-delete hooks (not checked for Argo CD
  or Flux) need the two-step uninstall.
- Nodes the DaemonSet no longer runs on (a changed node selector or
  toleration, a deleted node) are not cleaned.

## Alternatives considered

- **Only the two-step flow** (`helm upgrade --set plugin.install.action=remove`,
  wait, `helm uninstall`). No API access and no new RBAC, but every user
  must know the extra step, and one who forgets it gets today's behaviour.
  It stays as the building block of the hook and as the path for tools
  without hooks.
- **A preStop hook that removes when the DaemonSet is being deleted.**
  Unsafe. The pod stops on every re-roll, eviction and node shutdown, so the
  hook needs API access to tell an uninstall apart, and an upgrade that
  renames the DaemonSet deletes the old one while the new one installs the
  same provider name: the removal can then run after that install and
  leave the node without the entry. The removal restarts kubelet, which
  runs the hook and owns the pod's termination; a removal with its
  verification and rollback needs minutes, longer than the 30 s default
  grace period, which also bounds every node shutdown. After the
  DaemonSet is gone nobody sees an error, and Helm does not wait for it.
- **A second, removal-only DaemonSet as a pre-delete hook.** Helm does not
  wait for a hook DaemonSet (statuswait.go:78-90), so it needs the same
  waiting Job, and the install DaemonSet still exists: an install pod that
  restarts during the hook installs the entry again after the removal.
- **A post-delete hook.** The plugin namespace may be terminating by then
  when the chart created it, so the hook cannot create its DaemonSet there,
  and the release namespace may enforce Pod Security "restricted"
  (ADR-0027).
- **One Job per node from `lookup` at render time.** Helm renders hooks at
  install and upgrade, so the list is the nodes of the last upgrade, and
  `helm template` (GitOps) renders an empty `lookup`.
- **Change a field the chart does not render** (an annotation or an extra
  environment variable). A later `helm upgrade` keeps it under both merge
  methods, so a failed uninstall followed by an upgrade would leave the
  DaemonSet removing the plugin from every node.
- **kubectl or client-go in the image.** The installer stays off the k8s.io
  module curve (ADR-0021); kubectl adds a large binary and its CVEs for
  three API calls.
- **The DaemonSet's own pace (10%) for the removal.** 10% rounds up to one
  pod per wave up to ten nodes, so a cluster of seven or more nodes needs
  seven to ten waves, more than Helm's default timeout at about 45 s per
  wave (estimate). 25% needs at most four waves at any size. All nodes at
  once is fastest, but a removal that kubelet rejects then takes every node
  `NotReady` for the verification timeout at the same time.
- **Remove the environment-file line, the shared `.bak` copies or the lock
  files.** The installer cannot tell its own empty line from the one RPM
  packages ship; the `.bak` is the operator's only copy of the content
  before the removal; a removed lock file lets two installers hold "the"
  lock at once (lock.go:137-145).
- **Delete the binary in none mode.** Kubelet's flags may name a config the
  installer cannot see that names the binary; kubelet would not start
  after its next restart.
- **Renames through a pre-upgrade hook, or a `plugin.previousProviderNames`
  value.** The hook would remove the old name on every node before the
  upgrade installs the new one, so each node pulls without credentials in
  between; a value naming another release's provider by mistake would
  remove that release's entry, which its record vouches for.

- **A pre-upgrade hook that removes when `plugin.enabled` turns false.**
  The plugin may then come from a node image or a system extension under
  the same name and paths, which the removal would delete. The two-step
  change (`plugin.install.action=remove`, wait, then `plugin.enabled=false`)
  says what the operator wants.
- **Wait for every node before failing.** A node that reported a failed
  removal does not recover on its own (ADR-0033 refuses the same content
  again), so waiting only delays the report; the DaemonSet's rollout goes
  on without the Job.
- **Revert the hook's patch when it fails.** Every node it cleaned would
  install again and restart kubelet, and the next attempt would remove and
  restart once more.
