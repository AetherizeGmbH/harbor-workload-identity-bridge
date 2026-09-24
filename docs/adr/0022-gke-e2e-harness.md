# 22. GKE e2e harness for merge-mode installation

## Status

Accepted

## Context

ADR-0021 introduces `merge` mode: on managed nodes the installer
injects our provider entry into the cloud's existing
`CredentialProviderConfig` instead of patching kubelet flags. The kind
e2e cannot exercise this path — kind nodes have no pre-existing
credential-provider wiring, so `auto` always resolves to `patch`
there. Merge mode needs a real managed cluster, and GKE is the first
target (README roadmap item 1).

Constraints that shape the harness:

- **Kubernetes ≥ 1.34.** The plugin depends on KEP-4412's
  `serviceAccountToken` field; the gate
  `KubeletServiceAccountTokenForCredentialProviders` is beta and
  default-on since 1.34, and GKE does not allow setting custom kubelet
  feature gates. The cluster must therefore be ≥ 1.34.
- **No `kind load`.** Working-tree images must be pushed to a registry
  the cluster can pull from. Chosen: an ephemeral Artifact Registry
  repository in the same project — private, pulled via GKE's built-in
  GCP credential provider (which doubles as the coexistence probe, see
  below), torn down with the cluster. (ttl.sh would publish unreleased
  images; GHCR would pollute the public registry with dev tags.)
- **No CoreDNS.** The kind harness resolves `harbor.e2e` via a CoreDNS
  rewrite; GKE runs kube-dns. Instead Harbor is exposed through a
  `LoadBalancer` Service and addressed as `harbor.<LB-IP>.sslip.io` —
  resolvable by both containerd (host) and pods without touching
  cluster DNS.
- **Containerd trust is test scaffolding.** Harbor's cert is
  self-signed, so each node needs
  `/etc/containerd/certs.d/<host>/hosts.toml` (`hosts.toml` is read
  per pull — no containerd restart) and, if GKE's containerd config
  lacks a `config_path` stanza, a one-time `config.toml` append plus
  containerd restart. This is a test-only DaemonSet, never shipped:
  production users bring registries with real certs; registry TLS
  trust is orthogonal to what the bridge does.
- **Auth plumbing.** The existing OpenTofu modules take a
  client-cert kubeconfig (kind). GKE authenticates with a short-lived
  token from `google_client_config`. The shared modules accept either
  (client-cert fields optional, token optional).

## Decision

Add `test/e2e-gke/` as a second OpenTofu test root, reusing
`test/e2e/modules/*` where possible, run locally via `make e2e-gke`
(gated on `GOOGLE_PROJECT`; **not** part of CI — cost and credentials
are the operator's).

Harness shape:

- `modules/gke-cluster/`: zonal `google_container_cluster`
  (`deletion_protection = false`, minimum version 1.34, Dataplane V2),
  a single spot node pool sized for Harbor, plus an ephemeral
  `google_artifact_registry_repository`; outputs a token-auth
  kubeconfig.
- Working-tree images built by the existing `docker-build` module and
  pushed to the Artifact Registry repo.
- Harbor via the existing module with `service.type=LoadBalancer` and
  `externalURL=https://harbor.<LB-IP>.sslip.io`.
- Test-only containerd-trust DaemonSet as described above.
- Bridge installed with `plugin.install.mode=auto`, which on GKE must
  resolve to **merge**.

Load-bearing assertions, in order:

1. **Pull via the bridge** — a pod whose image lives in the in-cluster
   Harbor starts successfully (the full plugin → bridge → robot path,
   same as kind).
2. **Cloud coexistence** — a pod whose image lives in the private
   Artifact Registry repo *still* pulls successfully after the merge.
   This is the assertion that merge mode preserved GKE's own provider
   entry; it fails if the installer mangled the discovered config.
3. **Upgrade convergence** — re-install with a changed `matchImages`
   and assert a pull that only works if the installer detected the
   content change and restarted kubelet (mirrors the kind upgrade
   stage).
4. **Diagnostics** — dump the discovered config path/format and the
   installer logs into test output, so runtime findings feed back into
   this ADR and the README.

## Consequences

- Merge mode has a real proof on a managed cloud; EKS/AKS remain
  design-supported but unverified until analogous harnesses exist
  (future work — the module split keeps that cheap).
- The harness costs real money and needs `gcloud` auth; it is
  documented in HOW-TO-TEST.md and deliberately absent from CI. A
  manual-dispatch workflow can come later once the harness is proven.
- The `google_client_config` token expires after ~1h; the harness is
  expected to finish well within that, otherwise provider re-auth is a
  known follow-up.
- Runtime unknowns are contained: the GKE provider config path/format
  is discovered (never hardcoded), loopback-NodePort reachability
  under Dataplane V2 has the `plugin.bridgeEndpoint` / `$(NODE_IP)`
  escape hatch, and the containerd `config_path` question only affects
  test scaffolding.

## Alternatives considered

- **ttl.sh / GHCR for image delivery.** Rejected: publishes
  unreleased working-tree images (ttl.sh) or pollutes the public
  package registry (GHCR). Artifact Registry also gives the
  coexistence assertion for free.
- **Reusing the CoreDNS-rewrite trick.** GKE runs kube-dns; patching
  it is fragile and Google-managed. sslip.io needs no cluster DNS
  changes.
- **A real certificate (Let's Encrypt) for Harbor.** Avoids the
  containerd-trust DaemonSet but couples the harness to ACME rate
  limits and public DNS; sslip.io + skip-trust scaffolding is
  self-contained.
- **Running the GKE e2e in CI now.** Premature: needs a funded
  project, Workload Identity Federation setup, and confidence in the
  harness. Local-first, CI later.

## See also

- [ADR-0021](0021-node-installer-modes.md) — the install modes this
  harness verifies.
- [test/e2e/](../../test/e2e/) — the kind harness whose modules are
  reused.
- KEP-4412 / Kubernetes 1.34 release notes — beta default-on gate.
