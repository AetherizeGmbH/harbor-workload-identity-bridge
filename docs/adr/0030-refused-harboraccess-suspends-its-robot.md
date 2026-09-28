# 30. A refused HarborAccess suspends its robot

## Status

Accepted (2026-09-27). Supersedes the "stops without touching Harbor" part
of ADR-0009 §6. Amends ADR-0026 §1 (what "no robot" means for a
HarborAccess that already had one) and ADR-0023 (a disabled robot is the
administrator's decision unless the bridge marked it as its own
suspension). Extends ADR-0012 by one description token.

## Context

The reconciler refuses a HarborAccess it must not serve:
`IssuerMismatch` (ADR-0009), `AudienceMismatch` (ADR-0026) and
`InvalidSpec` (a missing spec, a name longer than 63 characters, a
project name that is not a Harbor project name, such as `*`, which
Harbor reads as every project, a `tokenTTL` that is not a Go duration, or
a `serviceAccountRef` that maps to no robot name Harbor accepts). It
returned before any Harbor call. That is right for a
HarborAccess that never had a robot, and wrong for one that did (audit
2026-09, control-plane findings 17, 38, 98):

- A HarborAccess becomes refused after it was served: its audience or
  issuer is edited, the bridge's audience or issuer changes, or an upgrade
  starts refusing what an older bridge accepted (0.6.0 pinned the audience,
  0.5.5 refused `*`).
- Its robot kept its grants and a valid password. Nothing rotated it
  again: rotation runs further down the same pass. The janitor kept it,
  because the owner exists, is selected and still maps to that robot name.
- The ADR-0003 bound is gone: a password leaked before the HarborAccess
  became refused, for example through the default-audience tokens that
  0.6.0 stopped accepting (M7), stays valid indefinitely.
- For `InvalidSpec` the data plane still matched the HarborAccess and kept
  handing out the robot's password. A robot created with `*` before 0.5.5
  kept push and pull on every project after the upgrade that was meant to
  remove that grant.
- The emergency rotation (delete the robot Secret) did nothing: the pass
  it triggers stopped at the same check.

Deleting the robot of a refused HarborAccess would close this, but the
same refusals fire for every HarborAccess at once when the bridge itself
is misconfigured (a wrong `BRIDGE_AUDIENCE` in an upgrade). A reversible
state is preferred over deletion.

## Decision

1. **Suspend.** When the reconciler refuses a HarborAccess with
   `IssuerMismatch`, `AudienceMismatch` or `InvalidSpec`, it first deletes
   the robot Secret, if the Secret belongs to the HarborAccess (the same
   ownership rule as for writes: stamped for it, or unlabelled and holding
   this robot's credentials) and, when it carries the bridge's labels, is
   stamped for this cluster. That needs only the apiserver, so it happens
   even when Harbor cannot be reached. A Secret stamped for another
   cluster belongs to a bridge that shares the namespace and is left
   alone. The reconciler then looks up the robot the HarborAccess's
   `serviceAccountRef` maps to. If that robot is owned by the HarborAccess
   (`robotOwnedBy`: this cluster's prefix and description tag, and the
   description names this HarborAccess) and is enabled, the reconciler
   disables it through Harbor's update (which echoes the on-wire name and
   sends the new `disable` flag) and appends the description token
   `suspended=true`.
2. **Delete when disabling is impossible.** Harbor's update replaces the
   robot's grants with the ones sent. When the robot carries a grant the
   bridge refuses to write (a project name that fails
   `harbor.ValidateProjectName`, such as a pre-0.5.5 `*`) or none it can
   express, the robot is deleted instead.
3. **An administrator's disable is left alone.** A robot that is already
   disabled without the token was disabled in Harbor; it is not marked.
   Fixing the HarborAccess later leaves it disabled and reported as
   `RobotDisabled` (ADR-0023).
4. **Resume.** When the HarborAccess is accepted again and its robot is
   disabled and carries the token, the reconciler deletes the robot Secret
   if one exists, rotates the password while the robot is still disabled,
   then re-enables the robot and converges its description and grants in
   one update, and then stores the new password. Harbor rotates the secret
   of a disabled robot and keeps it disabled (goharbor/harbor
   `src/server/v2.0/handler/robot.go` `RefreshSec` does not look at the
   flag; `src/controller/robot/controller.go` `Update` writes `disabled`
   back unchanged). A password handed out before the suspension therefore
   never works again, whatever step a pass fails at: before the enabling
   update succeeds the robot is disabled and carries the token (the next
   pass resumes again); after it, the Secret is missing (the next pass
   rotates). A password is handed out only after the robot is enabled, so
   it is never rotated early by a repeated resume.
5. **Re-checked.** A refused HarborAccess is reconciled again every
   `ResyncInterval` (less the per-object offset Ready objects get, so the
   passes do not all hit Harbor at once), so a suspended robot an
   administrator re-enables in
   Harbor is disabled again. If the suspension fails (Harbor unreachable),
   the condition keeps the refusal reason, says that suspending failed, and
   the pass is retried with backoff.
6. `RobotProvisioned` is set to `False` with the refusal reason, so the
   status no longer claims a usable robot. Its message says what happened
   to the robot: suspended, deleted, disabled by an administrator, none
   found, or the suspension failed and is retried.

The token is additive (ADR-0012): `RobotBelongsToCluster` and
`ParseRobotDescription` read the same `cluster=` and `harboraccess=`
tokens as before, so the janitor and older bridges still recognise the
robot.

## Consequences

- A refused HarborAccess has no usable robot and no robot Secret. The data
  plane has nothing to serve for it: it refuses `AudienceMismatch` and
  `IssuerMismatch` objects, and an `InvalidSpec` one whose `tokenTTL` is
  not a Go duration, at matching (403), and finds no Secret for any other
  `InvalidSpec` one (503).
- A wrong `BRIDGE_AUDIENCE` (or a wrong issuer that still lets the bridge
  start) now suspends every robot at once. Credentials kubelet cached stop
  working immediately instead of when their cache entry expires (at most
  `tokenTTL`); new requests were refused before, too. Correcting the value
  resumes every robot with a new password on the next pass. This is the
  price of closing the leak; it is reversible without recreating anything.
- Every pass over a refused HarborAccess costs one robot lookup, on each
  event and every `ResyncInterval`, even for one that never had a robot.
  A lookup that finds no robot falls back to listing every robot in
  Harbor (one call per 100 robots), so many refused objects that never
  had a robot (overlapping selectors, or every cluster's objects applied
  everywhere) cost many listings per hour.
- The janitor is unchanged. It keeps the suspended robot of an existing,
  selected HarborAccess, and deletes it once the HarborAccess is gone or no
  longer selected, like any other robot.
- Downgrading to a bridge without this decision: its next pass over an
  accepted HarborAccess rewrites the description without the token and
  echoes `disable`, so the robot stays disabled and is reported as
  `RobotDisabled` until an administrator re-enables it. Fail closed.
