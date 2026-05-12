# 11. Robot password storage as per-CR Kubernetes Secret in the bridge namespace

## Status

Accepted

## Context

Each persistent Harbor robot ([ADR-0003](0003-persistent-robots-per-harboraccess.md)) has a password that Harbor only returns on Create and RefreshSec. The bridge must store this password somewhere durable, accessible to the data plane (originally to call `/service/token` per ADR-0005; post-pivot, to return as Basic Auth per [ADR-0013](0013-return-robot-basic-auth-credentials.md) — same Secret-access requirement, different downstream use), and **not** accessible to the workloads themselves (which would otherwise bypass the bridge entirely and pull images directly with the robot's credentials).

Three orthogonal questions:

1. **Where does the Secret live** — in the HarborAccess CR's namespace, or in the bridge's own namespace?
2. **One Secret per CR, or one shared Secret with many keys?**
3. **What labels / metadata for operator observability?**

## Decision

**Per-CR Kubernetes Secret in the bridge namespace.**

- Name: `robot-<harboraccess-namespace>-<harboraccess-name>` (with `SecretNamePrefix = "robot-"`).
- Namespace: the bridge's own namespace, supplied via `BRIDGE_NAMESPACE` ([bridge/controlplane/config.go](../../bridge/controlplane/config.go)).
- Type: `corev1.SecretTypeOpaque`.
- Data keys: `username` (the Harbor robot's full name) and `password` (the secret). Two keys match the standard Kubernetes-Secret-as-volume convention so the future data plane can mount the Secret directly without parsing logic.
- Labels:
  - `harbor.aetherize.io/managed-by: harbor-workload-identity-bridge`
  - `harbor.aetherize.io/cluster: <cluster>`
  - `harbor.aetherize.io/harboraccess-namespace: <ha.namespace>`
  - `harbor.aetherize.io/harboraccess-name: <ha.name>`

RBAC: only the bridge ServiceAccount has read access to Secrets in this namespace. The Helm chart wires this restriction; ADR-0009 already requires this surface to be cluster-scoped to one trust domain.

`HarborAccessStatus.Robot.PasswordSecretRef` carries the Secret name (not the full namespaced path — the namespace is fixed). This makes the link discoverable via `kubectl get harboraccess -o yaml` but does not leak the contents.

## Consequences

- A workload ServiceAccount in the CR's namespace **cannot** mount or read the Secret. The only way to obtain credentials for the robot is to go through the data plane, which validates the SA token first ([ADR-0006](0006-oidc-validation-and-audience.md)). The robot password never reaches the workload.
- All robot credentials live in one namespace. Auditors look at one place; backup tooling backs up one namespace.
- One Secret per CR means rotations are independent and one bad rotation can't corrupt another CR's credentials. Cost: N Secrets for N CRs — small for any realistic deployment.
- Secret name has the form `robot-<haNs>-<haName>`. Kubernetes name limit is 253 chars; an HA in a 63-char namespace with a 63-char name fits comfortably. If real-world CR names overflow we mirror the hash-truncate pattern from [harbor/naming.go](../../bridge/controlplane/harbor/naming.go) — noted as a TODO in [reconciler.go](../../bridge/controlplane/reconciler.go) `secretNameFor`.
- Per-CR labels make `kubectl get secrets -n <bridge-ns> -l harbor.aetherize.io/cluster=<cluster>` enumerate every robot credential the bridge manages, which is the natural answer to "what does this bridge own."

## Alternatives considered

- **Secret in the HarborAccess CR's namespace.** Rejected. Any workload running in that namespace with `get secrets` RBAC (a common grant for CI/CD operators) could read the robot password directly, bypassing the data plane's audience and subject checks. The whole point of the bridge collapses.
- **One shared Secret with one key per CR.** Rejected. Updates would race (kubectl apply / kubectl patch semantics differ across data keys); a single bad write could clobber every CR's password. Independent per-CR Secrets give independent rotation and reconciliation.
- **Secret in a kube-system-style "namespace-wide privileged" namespace shared across many bridges.** Rejected. Defeats the multi-cluster isolation principle ([ADR-0009](0009-multi-cluster-topology.md)): each bridge owns one cluster's robots and one cluster's credentials; cross-bridge sharing is explicitly out of scope.
