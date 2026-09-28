# 33. The installer rolls back a kubelet restart that does not verify

## Status

Accepted, 2026-09-28. Refines ADR-0021 (node installer restart policy) and
ADR-0029 (per-install state and records); neither is reversed.

## Context

- The installer restarts kubelet after it changed the credential-provider
  config or, in patch mode, `/etc/default/kubelet`, and then verifies the
  restart (ADR-0021; audit M6). When the verification failed it returned
  the error and left the new files in place.
- Kubelet exits at startup when it cannot register the credential
  providers (`pkg/kubelet/kuberuntime/kuberuntime_manager.go`: "Failed to
  register CRI auth plugins", then `os.Exit(1)`). It does so for a config
  it cannot decode or validate (`pkg/credentialprovider/plugin/config.go`:
  a `defaultCacheDuration` that `time.ParseDuration` rejects or that is
  negative, a `matchImages` entry that does not parse as a URL host,
  `tokenAttributes` with the `KubeletServiceAccountTokenForCredentialProviders`
  gate off) and for a provider whose binary is missing. systemd starts it
  again (Restart=always) into the same failure, and the node stays
  `NotReady` until someone repairs it on the node.
- The chart renders `plugin.defaultCacheDuration` and `plugin.matchImages`
  into the entry without checking their syntax, so a value such as `1d`
  reached every node. A fresh install starts a DaemonSet pod on every node
  at once (control-plane nodes included, `tolerations: operator: Exists`);
  an upgrade stops after `maxUnavailable` (10%) nodes. A node whose
  kubelet is down cannot run the corrected pod, and `kubectl logs` of the
  failed install container goes through that kubelet.
- A verification can also fail while kubelet runs fine: in patch mode when
  the kubelet unit does not pass `/etc/default/kubelet` on to kubelet. The
  installer wrote its state only after a verified restart, so every retry
  of the init container (CrashLoopBackOff, with no limit for a DaemonSet)
  found no state for the content and restarted kubelet again. Kubelet keeps
  its container backoff in memory, which each restart drops, so the retries
  came about once per verification timeout.
- The `.bak` copies (`writeFileAtomic`) hold the content before the last
  write that changed a file, whoever wrote it: several installs share the
  config (ADR-0029), and the crash-window path of a pass restarts kubelet
  without writing anything.

## Decision

1. **Check this install's entry before writing it.** Every mode validates
   the rendered entry the way kubelet does for the fields the chart fills
   from values, with the standard library only (ADR-0021): `apiVersion`
   (`credentialprovider.kubelet.k8s.io/v1`, `v1beta1` or `v1alpha1`, and
   `v1` with `tokenAttributes`), a non-empty `matchImages` whose entries
   parse as `url.Parse("https://" + entry)` (kubelet's `ParseSchemelessURL`),
   a `defaultCacheDuration` string that `time.ParseDuration` accepts and that
   is not negative, and `tokenAttributes` with a non-empty
   `serviceAccountTokenAudience`, a boolean `requireServiceAccount` and a
   `cacheType` of `ServiceAccount` or `Token`. The pass refuses before it
   writes anything. Checks that depend on kubelet's version or feature
   gates are not repeated; the rollback covers them.
2. **Roll back a restart that does not verify.** Before it writes, a pass
   keeps the content of the files kubelet reads at startup that it may
   change, in memory, together with whether each existed: kubelet's
   config (merge mode) or this install's chart-owned config (patch mode),
   and `/etc/default/kubelet` (patch mode). When the verification after its
   restart fails, it writes those contents back (a file that did not exist
   is removed), rewrites its record (ADR-0029) to hold the entries it held
   before the pass plus the new one, as after an interrupted pass, restarts
   kubelet and verifies again, now only that kubelet stays up. The pass
   still fails and reports both results. It holds the node lock and the
   config locks throughout (ADR-0029, decision 5), so no other installer
   changed those files in between. The binary, the CA and the mTLS files
   stay: kubelet only needs the binary to exist, and it does. A pass that
   cannot record anything (an unwritable state directory) restores the
   files without restarting kubelet, which still runs what it read at its
   last start.

   A pass that did not change any file but restarts kubelet because its
   state does not record the content (the crash window of ADR-0021) has
   nothing of its own to restore. It restores nothing, says so, and names
   the `.bak` copies and `journalctl -u <unit>`. The `.bak` copies are not
   restored automatically, because another install or a cloud agent may
   have written the file after them.
3. **Do not apply rejected content again to the same kubelet.** After a
   failed restart or verification the pass records the content in its
   state file (`rejected`: mode, paths, the kubelet unit, `entryHash` and
   `appliedHash` of the rejected content, the kubelet, the error and the
   time), whether or not the rollback succeeded. The kubelet is a hash over
   the content of the binary at the path the running kubelet process was
   started from (`/proc/<pid>/exe`; after a package upgrade that path holds
   the binary the next restart runs), the process's command line without the
   two credential-provider flags, and the content of its `--config` file,
   taken when the pass records the rejection (after the rollback, when it
   restored anything). A later pass whose content has exactly these
   values refuses before it writes anything and does not restart kubelet;
   it names the recorded error and the way out: new content (a changed
   rendered entry, a changed shared file, a changed `/etc/default/kubelet`,
   another `plugin.install.kubeletUnit`), a changed kubelet, or deleting the
   state file on the node after fixing the node. A pass with the same
   content and a different kubelet tries it once more: the version- and
   gate-dependent rejections above end with a kubelet upgrade (the gate is
   beta and on by default since 1.34) or a gate turned on in the command
   line or the config file, which a pass that restarted another kubelet did
   not test. When either side cannot tell the kubelet (no single host
   kubelet process, a binary that is not a readable regular file), the
   rejection stays. A verified restart writes a new state without
   `rejected`. `schemaVersion` stays 1; installers that do not know the
   field ignore it.

## Consequences

- A value kubelet rejects costs each node it reaches two kubelet restarts
  and about one verification timeout (90 s) with kubelet down, longer than
  the node-monitor grace period (40 s, 50 s since Kubernetes 1.32): the node
  turns `NotReady` and its pods leave Service endpoints for that time. Then
  the node runs the previous config again and the pod stays in `Init:CrashLoopBackOff` with
  an error that `kubectl logs` can show. It no longer takes kubelet down
  until someone repairs the node. On a fresh install the first pass has no
  previous entry to restore: the node runs without this plugin.
- The same holds for a kubelet unit that does not pass the flags on in
  patch mode: two restarts, then no more until the content changes.
- A verification that failed for a transient reason (a node too slow to
  settle within the timeout) is not retried for the same content and the
  same kubelet. The error says how to retry: delete the state file on the
  node, or roll out new content.
- A kubelet upgrade, or another kubelet command line or `--config` file,
  gets one more try with rejected content by itself, without access to the
  node; when kubelet still rejects it, that costs one more rollback. A pass
  that finds a rejection for its content reads the kubelet binary once
  (about 100 MiB) to tell.
- After a rollback the `.bak` copy of each restored file holds the content
  kubelet rejected (the content before the last write, as always).
- The crash-window path keeps its gap: when an interrupted pass wrote
  content kubelet rejects, the next pass restarts kubelet onto it and
  cannot restore anything. It records the rejection, so it does not restart
  kubelet again.

## Alternatives considered

- **Restore from the `.bak` copies.** They are replaced by every write that
  changes a file, whoever makes it, and may predate another install's or a
  cloud agent's change; the bytes a pass read before its own write are
  exactly what it replaced.
- **Record an "attempted" state and re-verify without restarting on the
  next pass.** It stops the restart loop but leaves a config kubelet
  rejects in place, and kubelet down with it.
- **A retry budget per content (N restarts).** Bounds the loop but repeats
  a node outage N times for a deterministic failure. Possible later, if
  operators need automatic retries of a transient failure.
