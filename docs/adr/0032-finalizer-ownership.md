# 32. A bridge sets its finalizer right before the robot and releases only what it set

## Status

Accepted (2026-09-28). Amends ADR-0026 §3 and §4 (when a bridge sets its
finalizer and which finalizers it releases) and ADR-0030 (when a refused
HarborAccess costs a Harbor lookup).

## Context

Audit 2026-09, control-plane findings 34, 101, 107 and 108, and the review
of ADR-0030:

- The reconciler added its finalizer to every HarborAccess it selected,
  before any check. An object it refused (another bridge's audience with
  overlapping selectors, another cluster's issuer, an invalid spec) or
  found in `RobotConflict` got it too, although it never got a robot.
  Deleting such an object then waited for this bridge and a reachable
  Harbor (101).
- Deleting a HarborAccess deleted whatever Secret sat at its robot-Secret
  name unless it was stamped for another HarborAccess, including a
  foreign unlabelled Secret the reconciler had refused to adopt (107).
  `DeletionBlocked` named one finalizer when the object held two (108).
- Changing the selector stranded finalizers. A bridge that gained a
  selector never removed the shared `harbor.aetherize.io/robot` from
  objects that stopped matching; one that lost it never removed
  `harbor.aetherize.io/robot-<instance>` (34).
- ADR-0030 made every pass over a refused HarborAccess look its robot up
  in Harbor. A lookup that finds nothing lists every robot, and most
  refused objects never had a robot here.

## Decision

1. **The finalizer comes right before the robot.** The reconciler adds its
   finalizer after the refusal checks and the Secret-conflict check,
   immediately before it looks up or creates the robot. A robot of this
   bridge therefore never exists on an object without one of the
   finalizers this bridge sets or releases (`Config.ReleasedFinalizers`):
   bridges before this decision added the finalizer before anything
   else, so their robots are covered too.
2. **A refused HarborAccess costs Harbor nothing unless it holds such a
   finalizer.** The robot Secret is still deleted (ADR-0030 §1). The
   Harbor lookup and suspension run only while the object holds one of
   `ReleasedFinalizers`.
3. **A refused HarborAccess without a robot is released.** When the robot
   its `serviceAccountRef` maps to is absent, belongs to someone else or
   was just deleted (ADR-0030 §2), the robot Secret is gone, and a robot
   listing finds no other robot owned by the object, the bridge removes
   its finalizers with an optimistic lock. This releases the objects
   older bridges finalized while refusing them. A robot of an earlier
   `serviceAccountRef` or a pre-ADR-0018 robot keeps the finalizers until
   the janitor has deleted it. With a selector, only the per-instance
   finalizer is released this way: a bridge without a selector that
   serves the object sets the shared one too and would add it back at
   once. If the object is accepted again, decision 1 adds the finalizer
   back before any robot is created.
4. **Which finalizers are this bridge's** (`Config.ReleasedFinalizers`):
   with a selector, the per-instance one and the shared one (ADR-0026 §3);
   without a selector, the shared one and, when `BRIDGE_INSTANCE` is set,
   the per-instance one a selector since removed left behind. Deleting a
   HarborAccess removes every one of them the object holds, and
   `DeletionBlocked` names all of them.
5. **An object that stops matching** (ADR-0026 §4): once no robot of it is
   left in Harbor, the janitor removes the per-instance finalizer, and the shared one only if the object's
   `status.robot` names a robot of this bridge's `clusterName`, that is,
   this bridge served it last and set the shared finalizer before it had
   a selector.
6. **Deleting a HarborAccess deletes only its own robot Secret**: one the
   reconciler would write (stamped for the object, or unlabelled and
   holding this robot's credentials) and, when labelled, stamped for this
   cluster. Anything else stays, and the finalizer is released anyway.
7. **A robot the bridge cannot recognise by name counts as left.** A robot
   whose description names the object for this cluster but whose name is
   outside the ownership prefix (a configured robot prefix shorter than
   Harbor's, `misnamedRobot`) is never touched, and it keeps the
   finalizers under decisions 3 and 5 as a robot of the object would:
   releasing them would let the deletion skip a robot with a valid
   password. Deleting the object waits with `DeletionBlocked` until the
   prefix is corrected.

## Consequences

- A refused object that never had a robot here does not depend on this
  bridge to be deleted and costs no Harbor call. Objects an older bridge
  finalized while refusing them lose the finalizer on the first pass after
  the upgrade. Objects in `RobotConflict` that an older bridge finalized
  keep it until they are served or deleted.
- A refused object with a suspended robot costs one robot lookup per
  resync, which is what re-disables a robot re-enabled out of band.
- The suspension relies on the finalizer. An object whose finalizer was
  removed by hand while it lives, or that holds only the finalizer of a
  renamed instance, is not looked up when it is refused; its Secret is
  still deleted.
- Known limits, left open: the chart sets `BRIDGE_INSTANCE` only together
  with a selector, so after the selector is removed through the chart the
  bridge cannot name `robot-<instance>` and it must be removed by hand; a
  renamed instance never releases the old per-instance finalizer; decision
  5 keys on `clusterName`, which only bridges sharing a Harbor must keep
  distinct, so a selective bridge can release the shared finalizer of
  another bridge on the same cluster that uses the same `clusterName`
  with another Harbor; an object an older bridge refused or found in
  conflict has no `status.robot`, so if it stops matching a new selector
  its shared finalizer stays.
