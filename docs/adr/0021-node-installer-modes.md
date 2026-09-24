# 21. Node installer: Go binary with auto/merge/patch/none modes

## Status

Accepted

## Context

The plugin DaemonSet's install step is an inline `sh` script in
[charts/harbor-bridge/templates/plugin-daemonset.yaml](../../charts/harbor-bridge/templates/plugin-daemonset.yaml).
It copies the plugin binary, the rendered `CredentialProviderConfig`,
and the bridge CA to fixed host paths, then — when
`plugin.patchKubelet=true` (the default) — `nsenter`s into PID 1,
**overwrites** `/etc/default/kubelet` with
`KUBELET_EXTRA_ARGS="--image-credential-provider-bin-dir=…
--image-credential-provider-config=…"` and restarts `kubelet.service`.

Three problems:

1. **The idempotency guard checks flag *presence*, not config
   *content*** (`grep image-credential-provider-bin-dir
   /etc/default/kubelet`). Kubelet reads the credential-provider config
   file **once at startup** — there is no hot reload
   (kubernetes/kubernetes#130420 is an open feature request) — so after
   a `helm upgrade` that changes `matchImages`, `audience`, or
   `defaultCacheDuration`, the on-disk file is refreshed but kubelet
   keeps running the old config until an operator manually restarts it.
   This was the documented "helm upgrade caveat" in the README.
2. **The patch path breaks managed nodes.** EKS (AL2023), GKE, and AKS
   node images already run kubelet with `--image-credential-provider-*`
   flags for their own out-of-tree providers (`ecr-credential-provider`
   at `/etc/eks/image-credential-provider/`, `auth-provider-gcp`,
   `acr-credential-provider` under `/var/lib/kubelet/`). Overwriting
   `/etc/default/kubelet` on those nodes at best does nothing (the unit
   may not source it) and at worst clobbers provider-managed settings.
   The only escape hatch was `patchKubelet=false`, which drops files
   into paths kubelet never looks at unless the operator wires the
   flags out-of-band.
3. **Overwriting `/etc/default/kubelet` clobbers operator-set
   `KUBELET_EXTRA_ARGS`** even on self-managed nodes — a latent bug in
   the current script.

Facts the design rests on (verified against kubelet docs/source, July
2026, Kubernetes 1.33–1.35):

- `--image-credential-provider-config` and
  `--image-credential-provider-bin-dir` are **flag-only**. There is no
  `KubeletConfiguration` field and therefore no way to set them through
  the kubelet drop-in config directory (`--config-dir`, GA in 1.35).
  The live kubelet command line is authoritative for whether and where
  a node has credential providers wired.
- Kubelet parses the config file at startup, but **looks up and execs
  plugin binaries per image pull**. Consequence: if the flags and the
  config entry are already in place, replacing the plugin *binary*
  requires no kubelet restart; changing the *config content* or the
  *flags* always requires one.
- Restarting kubelet does not kill running containers — containerd
  owns them. A DaemonSet restarting kubelet via `nsenter`/`systemctl`
  is established practice (NVIDIA GPU operator and similar
  node-agent installers).
- The on-disk `CredentialProviderConfig` of cloud providers may be
  JSON (EKS `config.json`) or YAML (GKE, AKS). `sigs.k8s.io/yaml`
  parses both (YAML is a superset); emitting back in the format found
  preserves the file's ownership conventions.
- GKE COS mounts `/etc` writable but **ephemeral** — node re-creation
  (auto-upgrade, auto-repair, scale-up) wipes it. A DaemonSet
  re-applies naturally because a fresh node runs a fresh pod.
- Bottlerocket exposes no writable host filesystem (settings API
  only); installing a custom credential provider there is not
  feasible. Out of scope.

## Decision

Replace the inline shell script with a dedicated Go binary,
[installer/](../../installer/), shipped in the same image as the
plugin and run by the DaemonSet. It has four install modes, selected
by `plugin.install.mode`:

- **`auto` (default).** Read the live kubelet command line
  (`hostPID: true` makes the host's `/proc` visible; scan for
  `comm == kubelet`, parse `/proc/<pid>/cmdline`). If both
  `--image-credential-provider-*` flags are present → behave as
  `merge`. If absent → behave as `patch`. If neither mode can proceed,
  fail loudly (non-zero exit, actionable log) — never half-install.
- **`merge`** (managed clouds). Drop the plugin binary into the
  *discovered* `--image-credential-provider-bin-dir`. Load the
  discovered config file (JSON or YAML, detected by first non-space
  byte), treat it as `map[string]any` so foreign providers and unknown
  fields round-trip untouched, and replace-or-append (keyed by
  provider `name`) our single provider entry extracted from the
  chart-rendered config. Write atomically (same-dir temp + rename),
  keeping a `.bak` of the prior content. `plugin.install.binDir` /
  `plugin.install.configFile` override discovery when set.
- **`patch`** (self-managed: kind, kubeadm, k3s). Own bin/config dirs
  as today, but `/etc/default/kubelet` is **parse-merged**: existing
  `KUBELET_EXTRA_ARGS` are preserved, stale
  `--image-credential-provider-*` occurrences are stripped, ours are
  appended. Other lines in the file are kept verbatim.
- **`none`.** Files only — today's `patchKubelet=false`. No hostPID,
  no privileged container, no host-root mount; only the two narrow
  hostPath mounts.

**Restart policy (content-hash idempotency).** The installer persists
a state file on the host (`plugin.install.stateDir`, default
`/var/lib/harbor-bridge`) recording mode, effective paths, and sha256
hashes of the effective config content and kubelet flags. Kubelet is
restarted (`nsenter -t 1 -m -u -i -n -p -- systemctl restart <unit>`,
unit from `plugin.install.kubeletUnit`) **iff** the effective
config-file bytes changed, the patch-mode `/etc/default/kubelet`
content changed, or the recorded mode/paths changed. Binary drops and
CA/mTLS rotations never restart kubelet: binaries are exec'd per pull
and the CA is read by the plugin on every exec. This closes the helm
upgrade caveat.

**CA/mTLS files stay under `plugin.hostConfigDir` in every mode** —
the provider entry references them by absolute path via env vars, so
their location is independent of which config file kubelet reads.

**Sync loop for cert rotation.** The DaemonSet's long-running
container is no longer `pause` but the installer in `--sync` mode: it
watches the mounted Secret volumes (kubelet updates them in place) and
re-copies CA/mTLS files to the host when their content changes. This
fixes a real gap: the pod-template `tls-checksum` annotation hashes
only the Secret *name* and feature flags, so cert-manager's in-place
rotation never re-rolled the DaemonSet and nodes kept a stale CA until
the next unrelated re-roll.

**`plugin.patchKubelet` is removed, not shimmed.** The project is
pre-release alpha. The chart fails at template time with a pointer to
`plugin.install.mode` if the old key is still set.

**Hen-and-egg guardrail.** The plugin can only run after its image was
pulled, so the plugin/bridge images must not themselves require the
plugin. The chart fails at template time when the registry host of
`plugin.image.repository` or `bridge.image.repository` exactly matches
a `plugin.matchImages` entry (exact-host comparison only — kubelet's
glob semantics are not reimplemented in Helm), overridable with
`plugin.allowSelfMatchImages: true` for air-gapped mirrors that accept
the bootstrap ordering. A bridge outage affects only *new* pulls of
matched images (kubelet's credential cache covers the rest), and a
kubelet restart from the DaemonSet leaves running containers untouched.

**Isolation.** The installer must not import `k8s.io/*`;
`sigs.k8s.io/yaml` (and its `go.yaml.in` dependencies) is the only
permitted `sigs.k8s.io` import — it is needed to round-trip
cloud-provider config files. Enforced by
`make verify-installer-isolation`, alongside the untouched
`verify-plugin-isolation` (ADR-0015).

## Consequences

- Managed clusters (GKE, EKS AL2023, AKS) install with the default
  `mode: auto` and no manual kubelet wiring; the merge preserves the
  cloud's own provider. A managed-cluster e2e must verify this by
  asserting that cloud-registry pulls still work after install.
- `helm upgrade` changing `matchImages`/`audience` now converges
  without operator action: checksum annotations re-roll the DaemonSet,
  the installer detects the content change and restarts kubelet once
  per node. No-op re-rolls do not restart kubelet (state-hash match).
- The `auto`/`patch`/`merge` pod needs `hostPID`, `privileged`, and a
  writable host-root mount at `/host` (merge paths are unknown at
  render time). `mode: none` remains the least-privilege option.
  Documented in SECURITY.md; Trivy misconfig findings for this
  DaemonSet stay path-scoped and justified in `.trivyignore.yaml`.
- The installer is unit-testable (JSON/YAML merge, cmdline parsing,
  `/etc/default/kubelet` merging, hashing, atomic writes) — the old
  inline script was not.
- Node re-creation on GKE (ephemeral `/etc`) self-heals: the fresh
  node's DaemonSet pod reinstalls everything, including one kubelet
  restart (state file on `/var/lib` may also be gone; the resulting
  restart is on a node that is still cordon-free but empty, which is
  harmless).
- Bottlerocket is explicitly unsupported; the README says so.
- One more binary in the plugin image (still alpine-based — `nsenter`
  comes from the image).

## Alternatives considered

- **systemd drop-in (`/etc/systemd/system/kubelet.service.d/*.conf`)
  instead of `/etc/default/kubelet` for patch mode.** Rejected:
  kubeadm's `10-kubeadm.conf` loads
  `EnvironmentFile=-/etc/default/kubelet`, and systemd gives
  `EnvironmentFile` precedence over drop-in `Environment=` — a drop-in
  setting `KUBELET_EXTRA_ARGS` is silently defeated on exactly the
  nodes patch mode targets. Rewriting `ExecStart` in a drop-in would
  work but is far more invasive. Parse-merging the environment file
  keeps the current, e2e-proven mechanism and fixes its clobbering bug.
- **Keep the shell script and extend it.** Merging JSON/YAML provider
  configs, hashing content, and writing atomically in POSIX sh inside
  a Helm-templated heredoc is fragile and untestable. Rejected.
- **Import `k8s.io/kubelet` config types for the merge.** Rejected for
  the same reasons as ADR-0015, plus a stronger one: typed
  round-tripping *drops unknown fields* of other providers' entries,
  which is precisely what the merge must never do. `map[string]any` is
  the correct tool here.
- **Watch-and-reload instead of restart.** Kubelet cannot reload the
  credential-provider config (k/k#130420 open); no alternative exists.
- **A privileged one-shot Job instead of a DaemonSet.** Loses the
  re-apply-on-node-creation property that managed clouds (ephemeral
  `/etc`, autoscaling) depend on.
- **Deprecation shim for `plugin.patchKubelet`.** Mapping
  `patchKubelet: true → mode: patch` would silently keep the clobbering
  semantics users may have worked around. Pre-release alpha: a loud
  template-time failure with migration pointer is safer.

## See also

- [ADR-0015](0015-plugin-duplicates-wire-types.md) — the isolation
  discipline the installer inherits (with the `sigs.k8s.io/yaml`
  exception).
- [ADR-0016](0016-credential-provider-cache-key-type.md) — why the
  per-pull exec model matters.
- Kubelet credential provider docs and KEP-4412 — flag-only settings,
  startup-only config read, per-pull binary exec.
- kubernetes/kubernetes#130420 — open request for config hot-reload.
