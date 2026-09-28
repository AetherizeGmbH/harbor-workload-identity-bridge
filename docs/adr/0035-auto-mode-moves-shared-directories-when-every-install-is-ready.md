# 35. Auto mode moves shared directories once every install is ready

## Status

Accepted, 2026-09-28. Refines ADR-0021 (auto mode) and ADR-0029 (several
installs on one node, patch mode's refusal to move kubelet away from
other installs' entries); neither is reversed.

## Context

- Auto mode resolves to patch when kubelet's credential-provider flags
  point at the directories of this install's last verified patch-mode
  pass, according to its state file, and then moves kubelet when
  `plugin.hostBinaryDir` or `plugin.hostConfigDir` changed (audit #92).
- Patch mode refuses to move kubelet away from a config that holds another
  install's entry: kubelet reads one config from one bin dir, and the move
  drops that install's entry or its binary (ADR-0029). Auto mode then fell
  back to a merge pass into the old config.
- That merge pass recorded merge mode in the state file. From then on auto
  mode no longer took the flags for the install's own, never tried the
  move again, also after the other install had left, and ignored the new
  directory values for good, the symptom of #92. The pass also restarted
  kubelet although nothing kubelet reads had changed: the recorded mode
  differed.
- When every install on the directories changes them together, which is
  what patch mode's refusal asks for, each install's move is refused
  because of the others' entries, and kubelet never moves.

## Decision

1. **Stay a patch install on the old directories.** When auto mode cannot
   move its own install yet, it runs a patch-mode pass on the directories
   kubelet uses now, the ones its state file records, instead of a merge
   pass: the same chart-owned config composition, the same kubelet flags,
   and a state that still records patch mode on those directories. A pass
   with unchanged content restarts nothing, and every later pass tries the
   move again. Before that pass the checks of a merge pass into a
   chart-owned config apply: the old bin dir must not overlap
   `plugin.hostConfigDir`, and the old config must not be inside
   `plugin.hostConfigDir` unless it is the chart-owned config there.
2. **Put the files into the new directories as well.** After that pass it
   writes this install's binary and record into `plugin.hostBinaryDir` and
   its entry into the chart-owned config in `plugin.hostConfigDir`, as
   none mode does, and leaves kubelet alone; a directory that did not
   change already got them. Nothing reads these files until kubelet moves.
3. **A move carries the installs that are ready.** Patch mode, explicit or
   auto mode moving its own install, refuses to move kubelet only because
   of another install's entry that the config it moves kubelet to does not
   keep: that config keeps an entry of another name exactly when a record
   in the bin dir kubelet moves to vouches for it, next to an executable
   binary (`siblingIn`, ADR-0029). The check computes that config as the
   pass writes it. A writer of `plugin.hostConfigDir` cannot fake such an
   entry: the records are in the bin dir, which only root on the node
   writes (ADR-0029, decision 4).

When all installs on the directories change them, each earlier pass stays
and stages its files, and the last one finds every other install staged
and moves kubelet with all of them.

## Consequences

- A single install that changed its directories moves kubelet with one
  restart, as before. Installs that change their directories together
  move kubelet in the pass of the last one; until then kubelet stays on
  the old directories and every install keeps working.
- An install whose new directories differ from those of installs that
  stay on the old ones stays there as long as they do. Its state keeps
  recording patch mode, and a later pass moves kubelet once they have left
  or changed too. Its pod succeeds; the log says why kubelet did not move.
- The new directories hold this install's files before kubelet uses them,
  also after a change that is never completed; the files in the old
  directories stay after a move, as before (README, "Uninstalling").
- A state file that records merge mode on the old directories, written by
  an earlier installer's merge fallback after a directory change, is not
  taken for the install's own: the flags could also be an operator's.
  MIGRATION.md says how to move such nodes.

## Alternatives considered

- **Keep the merge fallback and record the pending move in the state
  file.** Keeps the move pending, but installs that change their
  directories together still block each other, and the fallback still
  restarts kubelet for nothing.
- **Fail the pass instead of falling back.** Visible, but installs that
  change together block each other for good, and the DaemonSet rollout
  stops on every such node.
- **Copy the other installs' binaries and entries into the new
  directories.** Moves at once, but one installer then writes files it
  does not own, in another install's version.
- **Take a merge-mode state on a chart-owned config for the install's
  own.** Would also move the nodes of the last consequence by itself, but
  takes over flags an operator set for a none-mode install's config, and
  on a node whose kubelet unit reads no environment file the installer can
  edit it turns a working merge into a failing pass.
