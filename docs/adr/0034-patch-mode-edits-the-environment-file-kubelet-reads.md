# 34. Patch mode edits the environment file the kubelet unit reads

## Status

Accepted, 2026-09-28. Refines ADR-0021 (node installer, patch mode) and
corrects one of the facts it rests on; ADR-0021 is not reversed.

## Context

- Patch mode adds the two `--image-credential-provider-*` flags to
  `KUBELET_EXTRA_ARGS` in `/etc/default/kubelet` and restarts kubelet
  (ADR-0021). The path was a constant. ADR-0021's alternatives section
  says that kubeadm's `10-kubeadm.conf` loads
  `EnvironmentFile=-/etc/default/kubelet`.
- That holds for the Debian packages only. In kubernetes/release (the
  source of pkgs.k8s.io), `cmd/krel/templates/latest/kubeadm/10-kubeadm.conf`
  has `EnvironmentFile=-/etc/sysconfig/kubelet`; `kubeadm.spec` rewrites it
  to `/etc/default/kubelet` only for Debian builds (`debbuild`), and
  `kubelet.spec` ships `/etc/sysconfig/kubelet` with an empty
  `KUBELET_EXTRA_ARGS=` for RPM builds. The kubeadm documentation says the
  same ("`/etc/default/kubelet` (for DEBs), or `/etc/sysconfig/kubelet`
  (for RPMs)").
- On RHEL, Rocky, Alma, Fedora and self-managed Amazon Linux kubeadm nodes
  the default mode `auto` resolves to patch (kubelet has no
  credential-provider flags), wrote a `/etc/default/kubelet` nothing reads,
  restarted kubelet and failed its verification: a half-install that
  ADR-0021 ("never half-install") rules out, found only after a kubelet
  restart.
- systemd reads the files of `EnvironmentFile=` in order; a variable set in
  a later file overrides an earlier one, and every file overrides
  `Environment=` (systemd.exec). `systemctl show -p EnvironmentFiles` lists
  the files of a unit, one `EnvironmentFiles=<path> (ignore_errors=yes|no)`
  line each, in that order.

## Decision

1. **Ask systemd which files the unit reads.** Before it writes anything,
   patch mode (also when `auto` resolves to it) runs
   `systemctl show --property=EnvironmentFiles --property=Environment <unit>`
   through `nsenter` and reads each listed file that exists, as systemd
   reads it. `systemctl show` prints a wildcard expression of
   `EnvironmentFile=` unexpanded; systemd expands it with `glob(3)` in the
   C locale (`safe_glob`), so the installer expands it the same way:
   matches sorted bytewise, a leading `.` matched only by a literal `.`,
   `[!…]` and `[^…]` negated. It refuses an expression with a character
   class, equivalence class or collating symbol, which it does not
   evaluate, and a symlink among the matched directories, which systemd
   follows and the installer does not. A file systemd skips assigns
   nothing: behind a `-` setting (`ignore_errors=yes`) a directory, or a
   file with a NUL byte or with a name or value that is not valid UTF-8,
   which systemd does not load at all. Without `-` such a file keeps the
   unit from starting, and the installer refuses it; so it does when the
   file is one it would edit.
2. **Edit only an operator file, and the one that counts.** The installer
   writes only `/etc/default/kubelet` or `/etc/sysconfig/kubelet`, never
   another file the unit names (kubeadm regenerates
   `/var/lib/kubelet/kubeadm-flags.env` on join and upgrade). It edits the
   last listed file that assigns `KUBELET_EXTRA_ARGS`, read as systemd
   reads it, because that assignment is the one kubelet gets; when no
   listed file assigns it, the last listed of the two operator files. It
   refuses, before any write or restart, when the unit lists neither
   operator file, when the assignment that counts is in another file, or
   when only `Environment=` sets `KUBELET_EXTRA_ARGS` (a line in the file
   would override it and drop its args). The refusal points to
   `plugin.install.mode=none`.
3. **The path is part of the recorded state.** The state file records the
   environment file (`envFile`), which the restart decision compares
   together with the mode and the paths: the content hash covers the
   file's bytes, not its name. A record without `envFile` is from patch
   mode on `/etc/default/kubelet` and matches it, so an upgrade restarts no
   kubelet. The post-restart verification stays: a unit can list the file
   and still not pass `$KUBELET_EXTRA_ARGS` to kubelet.

## Consequences

- RPM-packaged kubeadm nodes install with the default `mode: auto`; the
  flags go into `/etc/sysconfig/kubelet`, whose empty
  `KUBELET_EXTRA_ARGS=` line the installer fills.
- Debian-packaged kubeadm and kind nodes keep `/etc/default/kubelet`;
  nothing moves and no kubelet restarts on upgrade.
- A unit that reads neither file now fails before the installer writes
  anything or restarts kubelet, with a pointer to `mode: none`.
- The installer runs one more `systemctl` command per patch-mode pass.
  `kubeletControl` gains a method; the unit-name check applies to it.

## Alternatives considered

- **A chart value for the path.** An operator would have to know the
  packaging of every node; a cluster that mixes Debian and RPM nodes could
  not be served by one value. Discovery keeps `auto` zero-config. An
  override limited to the same two paths remains possible.
- **Write both files.** A file the unit does not read changes nothing, and
  writing it is the stray file this ADR removes; a file it does read may
  hold the assignment that counts.
- **A systemd drop-in setting `Environment=KUBELET_EXTRA_ARGS`.** Defeated
  by every `EnvironmentFile=` that assigns the variable (ADR-0021).
