# 12. Robot description as the cross-component reconciler↔janitor contract

## Status

Accepted

## Context

The Control Plane has two cooperating components: the reconciler (writes) and the janitor (sweeps). They need to agree, via Harbor itself, on:

1. **Is this robot ours?** — both must reject any robot that isn't the bridge's, for both cluster and authorship reasons. The ownership-prefix check from [ADR-0009](0009-multi-cluster-topology.md) is the first line of defense, but its known prefix-collision class means we need a second.
2. **Which HarborAccess CR does this robot belong to?** — the janitor needs to look up the CR by name to decide whether the robot is orphaned. The reconciler only knows from the *current* CR; the janitor sees Harbor state divorced from CR state.

Both questions need an answer storable on the robot itself, because Harbor is the shared bus between cluster instances.

Harbor does not expose a native "labels on robots" concept. The only places to stash structured metadata on a robot are its `name` (regex-restricted, also used by the ownership-prefix check) and `description` (varchar(1024), human-readable). We had to decide what to put in `description`.

## Decision

**The robot description is a stable, space-separated `key=value` token list, written by the reconciler and parsed by the janitor.** Format:

```
managed-by=harbor-workload-identity-bridge cluster=<cluster> harboraccess=<haNamespace>/<haName>
```

Helpers in [bridge/controlplane/labels.go](../../bridge/controlplane/labels.go):

- `RobotDescription(cluster, haNs, haName)` writes the format.
- `RobotBelongsToCluster(desc, cluster)` answers question 1 — defense-in-depth on top of `harbor.OwnsRobot(cluster, name)`. Catches the prefix-collision class from ADR-0009 even when the name itself accidentally matches.
- `ParseRobotDescription(desc)` answers question 2 — surfaces `(haNs, haName, ok)`.

The reconciler writes the description on Create and on UpdatePermissions. The janitor reads it on every sweep. Adoption discipline in the reconciler refuses to manage a robot whose description does not have the `cluster=<our cluster>` tag, even when the name prefix matches.

## Consequences

- The format is **a stable interface** between the reconciler and the janitor. Changing it is a compatibility break for any janitor older than the change running against robots written by a newer reconciler. Evolution rule: only add new tokens, never reorder or remove existing ones. The `managed-by=harbor-workload-identity-bridge` token is the version-anchor — its constant presence is what `RobotBelongsToCluster` checks first.
- The format is **legible in the Harbor UI and audit log**. Operators looking at a robot directly in Harbor can read off which cluster owns it and which CR backs it without consulting bridge logs. This is important for incident response when the bridge itself is unhealthy.
- The cluster tag is the second of two safety layers ([ADR-0009](0009-multi-cluster-topology.md)). A bridge in cluster `prod` reconciling a CR whose computed robot name happens to begin with `bridge-prod-` *and* whose pre-existing description carries `cluster=prod-eu` will refuse to adopt — `RobotConflict` condition, no Harbor writes.
- The format eats some of Harbor's 1024-char description budget. With cluster, namespace, and name each up to 63 chars (DNS labels), the description is well under 200 chars in practice. Plenty of headroom for additive tokens.
- The janitor's reverse-mapping is O(1) per robot (just a `strings.Cut` walk over tokens). No reflection, no JSON parse.

## Alternatives considered

- **Encode owner info in the robot name.** Rejected. The name is already a multi-cluster naming contract ([ADR-0009](0009-multi-cluster-topology.md)) constrained by Harbor's segment regex and the 255-char column cap. Adding "find the CR by parsing the name" couples two distinct concerns (cluster-scoped uniqueness vs. CR-back-reference) into one string with one length budget. The description is unconstrained and human-readable.
- **Annotation-style structured JSON in the description.** Rejected. JSON in a description that operators read in Harbor's UI is hostile to humans. The current key=value form is greppable, line-friendly, and parseable in one `strings.Fields` walk; the structured benefit (typed schema validation) is not worth the operator-UX cost given how short the payload is.
- **Reverse-map via Harbor labels.** Rejected. Harbor robots do not have a native label API — only the description field carries free-form metadata. Inventing a label-emulation system would mean another contract to maintain.
- **Store the mapping in a separate Kubernetes ConfigMap and skip the description entirely.** Rejected. The ConfigMap state could drift from Harbor's state under partial failures (Harbor robot created, ConfigMap update fails) and the bridge would have no way to tell from Harbor alone which robot is which. Putting the marker on the robot itself makes Harbor the single source of truth for "which CR does this belong to."
