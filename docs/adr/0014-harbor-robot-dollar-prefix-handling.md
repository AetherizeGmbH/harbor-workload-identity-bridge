# 14. Handle Harbor's `robot$` prefix asymmetry at the comparison boundary

## Status

Accepted

## Context

Harbor's robot-account API has an undocumented asymmetry between its
write and read paths for system-level robots:

- `POST /robots` accepts a `name` field in the request body without any
  prefix (e.g. `bridge-dev-test-pull-image-puller`) and Harbor stores
  the robot under that name.
- `GET /robots` and `GET /robots/{id}` both render the same robot's
  name with a literal `robot$` prefix prepended (e.g.
  `robot$bridge-dev-test-pull-image-puller`).
- A subsequent `POST /robots` with the same un-prefixed name returns
  `409 Conflict` (Harbor detected the collision against its stored
  prefixed form).

This was discovered empirically during the first end-to-end test
([HOW-TO-TEST.md](../../HOW-TO-TEST.md), commit `2d0ee96`). The
bridge's `harbor.RobotName()` returns the un-prefixed form, sends it
to Create, and every internal comparison uses the un-prefixed form.
The first reconcile succeeded (Create returned 201, the response
payload's `Name` happened to be the un-prefixed input echoed back).
Every subsequent reconcile called `GetByName`, which compared the
un-prefixed search string against the prefixed names in the List
response, missed, took the create branch again, and got 409 from
Harbor. The CR ended up in an inconsistent state
(`Ready=False` + `RobotProvisioned=True`).

A 409-on-create recovery path was added before the asymmetry was
understood (commit `a856598`). It also relied on `GetByName`, so it
also missed, and the recovery itself couldn't recover.

The asymmetry is not in any Harbor public API documentation we could
find. It is consistent across Harbor 2.x versions we tested and aligns
with [Harbor source `src/common/rbac` and the robot-account display
logic in `src/server/v2.0/handler/robot.go`](https://github.com/goharbor/harbor),
but Harbor may change the behaviour in a future release.

Three options for handling this:

1. **Normalise inbound at the SDK boundary.** Strip `robot$` from
   every `Robot.Name` returned by `List` / `GetByID` so internal
   code always sees the un-prefixed form. Re-add the prefix when
   constructing the Basic Auth username for the per-CR Secret.
2. **Normalise outbound.** Internally store and compare the
   prefixed form everywhere. `RobotName` would return the prefixed
   string and `Create` would strip the prefix only when serialising
   the SDK request body.
3. **Normalise at comparison points.** Keep the SDK shape unchanged;
   make `OwnsRobot` and `GetByName` accept either form by stripping
   the prefix during comparison only.

## Decision

Option 3 — normalise at comparison points.

A single constant `harbor.HarborRobotPrefix = "robot$"` is defined in
[bridge/controlplane/harbor/naming.go](../../bridge/controlplane/harbor/naming.go).
Comparison logic that takes a name from Harbor and matches it against
an internally-constructed name strips this prefix once, in exactly two
places:

- `OwnsRobot(cluster, robotName)` `strings.TrimPrefix`'s
  `HarborRobotPrefix` before the `ClusterPrefix` check.
- `(*goClient).GetByName(ctx, name)` matches each listed robot as
  `r.Name == name || r.Name == HarborRobotPrefix+name`.

`FilterOwned` delegates to `OwnsRobot` rather than re-implementing the
prefix check, so the normalisation has exactly one definition.

The per-CR Secret stores `robot.Name` verbatim as `username`. Because
we read `robot.Name` from Harbor's List/Get on the recovery path, the
Secret's `username` is the Harbor-on-wire prefixed form
(`robot$bridge-…`). Containerd needs this exact form to authenticate
against Harbor — `robot$` is part of Harbor's username, not a display
flourish — so storing it as-returned is the correct end state.

`ha.Status.Robot.Name` likewise carries the prefixed form, matching
what an operator sees in the Harbor UI.

## Consequences

- **The bridge tolerates the asymmetry.** If Harbor changes the read
  shape in a future release (drops the prefix, changes its delimiter)
  the *either-form* match in `GetByName` keeps the bridge working
  during the transition.
- **No data-shape divergence.** Internal storage holds the on-wire
  form Harbor returned. There is no second source of truth to keep
  in sync.
- **Future Harbor robot kinds may need attention.** Project-scoped
  robots use the prefix `robot$<project>+` (per Harbor source). The
  bridge only manages system-level robots today
  ([ADR-0009](0009-multi-cluster-topology.md)); if we ever add
  project-scoped robots, `HarborRobotPrefix` and the comparison
  helpers would need a second variant.
- **Test fixtures must mirror the asymmetry.** The unit-test
  `fakeHarbor` gained an `addRobotDollarPrefix` knob that prepends
  `robot$` on its GET handler. Tests covering `GetByName` and
  `OwnsRobot` use this knob so a regression that re-introduces the
  bug fails locally before it hits a real Harbor.

## Alternatives considered

- **Option 1 (strip inbound).** Cleaner internal model but
  introduces a data-shape difference between "what the bridge tracks"
  and "what Harbor returned". The Secret's `username` field would
  need re-prefixing at write time, and `ha.Status.Robot.Name` would
  no longer match what operators see in the UI. Rejected for the
  observability friction.
- **Option 2 (prefix-always).** `RobotName` would have to round-trip
  through the prefixed form for callers that need the wire shape
  (Create body) vs the comparison shape (everywhere else). Two
  representations sprinkled through the reconciler. Rejected for
  noise.
- **Detect prefixing dynamically at Client construction time.** Probe
  Harbor with a one-time Create+List, observe whether the prefix
  appears, configure the client accordingly. Too clever, adds a
  startup dependency on Harbor being reachable, and the cost of
  always tolerating both forms is essentially zero. Rejected.

## See also

- [bridge/controlplane/harbor/naming.go](../../bridge/controlplane/harbor/naming.go)
  — definition of `HarborRobotPrefix` and `OwnsRobot`.
- [bridge/controlplane/harbor/client.go](../../bridge/controlplane/harbor/client.go)
  — `GetByName` and `FilterOwned`.
- [bridge/controlplane/harbor/client_test.go](../../bridge/controlplane/harbor/client_test.go)
  — `TestClient_GetByName_ToleratesHarborRobotDollarPrefix`.
- [HOW-TO-TEST.md](../../HOW-TO-TEST.md) — the first end-to-end run
  that surfaced the bug.
