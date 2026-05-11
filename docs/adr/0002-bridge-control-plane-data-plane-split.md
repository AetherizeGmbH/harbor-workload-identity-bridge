# 2. Bridge split into Control Plane and Data Plane

## Status

Accepted

## Context

Upstream Harbor does not yet support OIDC Trust Policies on robot accounts ([goharbor/harbor#17520][upstream]). Until it does, workloads on Kubernetes cannot use service-account tokens to authenticate to Harbor directly. This project closes the gap with a server-side bridge.

That bridge has two responsibilities:

1. Provision and rotate Harbor robot accounts derived from `HarborAccess` CRs — a Kubernetes-API reconciler.
2. Validate SA tokens from the kubelet credential-provider plugin and mint Harbor docker bearer tokens — an HTTPS server fronted by no Kubernetes resource at all.

Responsibility 2 disappears entirely when upstream Harbor lands #17520. Responsibility 1 gains a "write trust policy to Harbor" step but does not disappear.

## Decision

The bridge is split into two Go packages inside one binary:

- `bridge/controlplane` — controller-runtime Reconciler for `HarborAccess`. Survives the upstream migration.
- `bridge/dataplane` — HTTPS server that validates SA tokens and mints docker bearer tokens. Will be deleted entirely after the upstream migration.

Both packages compile into a single binary (`bridge/cmd/main.go`). The split is logical, not deployment-level: running them as one process lets the data plane share controller-runtime's informer cache, which simplifies cache invalidation (see [ADR-0007](0007-cache-invalidation-on-cr-change.md)).

Package-import discipline: `controlplane` MUST NOT import `dataplane`. A lint rule (or a `go list -deps` check in CI) enforces this so the delete-at-migration is a single-PR change.

## Consequences

- Data-plane code lives in a clearly-named directory deletable as a single commit at migration time.
- Single process means a single `Deployment`, but two distinct liveness/readiness concerns. Both must report healthy for the binary to be Ready.
- After migration, the Deployment runs only the control plane. The Helm chart removes the Service, Certificate, and port exposure in one upgrade.

## Alternatives considered

- **One package, no split.** Rejected: makes the migration a refactor instead of a deletion, and conflates two very different code styles (reconciler vs HTTP handler).
- **Two binaries from day one.** Rejected: doubles deployment surface area now in exchange for zero benefit; the data plane vanishes entirely later anyway.

[upstream]: https://github.com/goharbor/harbor/issues/17520
