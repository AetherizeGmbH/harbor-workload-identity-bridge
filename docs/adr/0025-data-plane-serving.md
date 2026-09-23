# 25. Data plane serves on every replica; metrics leave the credential listener

## Status

Accepted. Amends ADR-0002 (process composition) and ADR-0008 (NodePort transport).

## Context

- The data-plane HTTPS server was added to the controller-runtime manager as a
  plain `Runnable`. A Runnable that does not implement
  `LeaderElectionRunnable` is started **on the leader only**. With the chart
  default of two replicas, the follower never bound `:8443`, yet passed
  readiness (an unconditional ping), so the NodePort Service sent about half of
  all credential requests to a closed port (audit H1). The e2e ran one replica
  and could not see it.
- `/metrics` shared the credential listener, which the NodePort exposes on every
  node's IP. Metric labels carry no identities, but operational data should not
  be reachable from wherever node ports are.
- The serving key pair was re-read from disk on every TLS handshake: two file
  reads per handshake on an unauthenticated listener, and a rotation caught
  half-way could pair a new certificate with the old key. The mTLS client CA was
  read once and never reloaded.

## Decision

1. `dataplane.Server` implements `NeedLeaderElection() = false`: every replica
   serves. The janitor and the reconciler stay leader-only.
2. Readiness is `Server.ReadyCheck`: ready while the credential listener is
   bound. Non-leader runnables start only after the informer caches have synced,
   so a ready replica can also answer the HarborAccess lookup.
3. `/metrics` moves to controller-runtime's metrics server on its own port
   (`BRIDGE_METRICS_ADDR`, default `:8080`, `"0"` disables), served over plain
   HTTP to the pod network through a separate ClusterIP Service; the chart's
   ServiceMonitor scrapes that Service. The NodePort Service exposes only the
   credential port.
4. The serving pair comes from controller-runtime's `certwatcher` (file events
   plus a 10s poll; it swaps in only a pair that parses as matching). The client
   CA bundle is reloaded on the same interval and kept on the last good version
   when a read fails mid-rotation. Certificate rotation needs no pod restart.
5. The credential response carries `Cache-Control: no-store`.
6. The e2e harness runs the bridge with two replicas, like the chart default.

## Consequences

- Credential issuance survives a leader change and the loss of any single
  replica; the chart adds a PodDisruptionBudget (`maxUnavailable: 1`) when
  `replicas > 1`.
- Scraping moves from `https://…:8443/metrics` to `http://…-metrics:8080/metrics`;
  users of the chart's ServiceMonitor need no change, custom scrape configs do
  (MIGRATION.md).
