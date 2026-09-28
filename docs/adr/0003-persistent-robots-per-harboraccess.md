# 3. Persistent Harbor robots per HarborAccess CR

## Status

Accepted. **Amended and in parts superseded; the code follows the later
ADRs:**

- Robots are named `bridge-<cluster>.<saNs>.<saName>`
  ([ADR-0009](0009-multi-cluster-topology.md),
  [ADR-0018](0018-dot-delimited-naming.md)), not `bridge-<namespace>-<sa>`.
- The data plane mints no docker tokens: it hands the robot's Basic Auth
  credentials to kubelet, so the password does leave the bridge
  ([ADR-0013](0013-return-robot-basic-auth-credentials.md), which supersedes
  ADR-0005).
- The password rotates on the 24h schedule or when the robot Secret or the
  robot is lost, never on a spec change; Harbor applies permission changes
  to the existing robot
  ([ADR-0023](0023-level-triggered-robot-lifecycle.md)).
- Harbor robots carry no labels. The bridge recognises its robots by the
  ownership prefix and the tokens in the robot description
  ([ADR-0012](0012-robot-description-as-component-contract.md),
  `bridge/controlplane/contract.go`) and reports a robot it does not own as
  `RobotConflict`.
- Deletion waits until Harbor confirms it (`DeletionBlocked`), the janitor
  revokes orphaned robots (ADR-0023), and a refused HarborAccess has its
  robot disabled
  ([ADR-0030](0030-refused-harboraccess-suspends-its-robot.md)).

## Context

For every `HarborAccess` CR we need a Harbor robot account that holds the configured pull/push permissions on the right projects. Two options:

1. **Ephemeral robots.** Create a robot per kubelet credential request, issue a docker token, delete the robot.
2. **Persistent robots.** Create one robot per `HarborAccess` CR; reuse across kubelet requests; rotate password on schedule or on permission change.

## Decision

We use persistent robots, one per `HarborAccess` CR, named `bridge-<namespace>-<sa>`. The Control Plane reconciler is the sole owner of each robot's lifecycle (create, password rotate, delete via finalizer). The Data Plane never creates or deletes robots — it only uses the existing robot's credentials to mint short-lived docker tokens.

Password rotation occurs every 24h and whenever the CR's permissions change (detected via `spec` generation bumps).

Adoption discipline: the reconciler refuses to manage a Harbor robot it did not create. It identifies its own robots by a Harbor label (`harbor.aetherize.io/managed-by=bridge`) plus the naming convention. A pre-existing robot with the bridge's name but no label produces a `Conflict` condition on the CR rather than silently being taken over.

## Consequences

- The blast radius of a leaked robot password is bounded by the rotation interval (24h), not by request duration. We mitigate further by never letting the password leave the bridge process — see [ADR-0005](0005-docker-token-via-service-token.md).
- One Harbor audit-log entry per CR rather than per pod start. Audit logs stay legible.
- Robot deletion needs a CR finalizer so deletion of the CR removes the Harbor robot. Orphan robots are visible (audit log) and recoverable manually.
- The shape mirrors upstream Harbor's eventual OIDC Trust Policy feature (a stable robot bound to a trust policy), making the migration path mechanical rather than architectural.

## Alternatives considered

- **Ephemeral robots per request.** Rejected:
  - Harbor robot create/delete is not cheap; image pulls bottleneck on Harbor admin-API throughput.
  - Failure mode at pod start is fatal: a brief Harbor outage fails every new pod. Persistent robots + docker-token cache survive transient Harbor outages.
  - Robot sprawl makes audit logs and Harbor UI unusable.
  - Does not match the upstream model we plan to migrate to.
