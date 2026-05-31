# 15. The plugin duplicates wire types instead of importing them

## Status

Accepted

## Context

The kubelet image-credential-provider plugin
([plugin/main.go](../../plugin/main.go),
[plugin/bridge_client.go](../../plugin/bridge_client.go)) sits between
two JSON wire protocols:

- **Kubelet → plugin** (stdin/stdout): `credentialprovider.kubelet.k8s.io/v1`
  (KEP-4412). Types upstream at
  `k8s.io/kubelet/pkg/apis/credentialprovider/v1` — `CredentialProviderRequest`,
  `CredentialProviderResponse`, `AuthConfig`, the `PluginCacheKeyType` enum,
  and `metav1.Duration`.
- **Plugin → bridge** (HTTPS): `POST /v1/credentials` with the
  `Request`/`Response` structs defined in
  [bridge/dataplane/handler.go](../../bridge/dataplane/handler.go).

The reflexive engineering instinct on both sides is "use the existing
types". For the kubelet side that's `import
k8s.io/kubelet/pkg/apis/credentialprovider/v1`. For the bridge side
that's `import github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane`
and reference `dataplane.Request` / `dataplane.Response` /
`dataplane.CredentialsPath` directly.

Two problems with importing upstream `k8s.io/kubelet`:

1. **Enum mismatch on `cacheKeyType`.** The bridge emits
   `cache_key_type: "ServiceAccount"` to signal that kubelet should key
   its credential cache on the SA identity (the KEP-4412 beta model).
   Upstream `PluginCacheKeyType` exposes `Image`, `Registry`, and
   `Global` as exported constants. Whether `ServiceAccount` exists as
   a named constant depends on which kubelet module version is pinned
   — older modules omit it, newer ones add it under the KEP-4412
   alpha/beta lifecycle. Importing the typed enum means a `go.mod`
   bump can silently break the wire contract; using the raw string
   never can.
2. **Transitive dep weight.** `k8s.io/kubelet` pulls in `k8s.io/api`,
   `apimachinery`, and (depending on the package subset)
   parts of controller-runtime. The plugin ships as a static distroless
   `nonroot` binary that runs on every node and is invoked
   synchronously inside kubelet's image-pull path. Smaller binary +
   smaller dependency surface = smaller attack surface, faster cold
   exec, less to audit.

Two problems with importing `bridge/dataplane`:

1. **Forces controller-runtime and `k8s.io/api` into the plugin's
   transitive closure.** `bridge/dataplane` imports
   `sigs.k8s.io/controller-runtime/pkg/client` for the handler's
   K8s API access. Even though the plugin would only reference
   `Request`, `Response`, and `CredentialsPath` — three plain
   structs and a string constant — the Go compiler treats the import
   as the whole package. Verified by `go list -deps` before this
   decision: importing `dataplane` added ~40 transitive packages.
2. **ADR-0002 spirit.** The control plane and data plane are
   intentionally split so each side can evolve under its own
   constraints. The plugin is a third compartment — same logical
   project, separate binary, separate deployment cadence, separate
   blast radius (runs as root on every node). Coupling its wire
   format to either internal package would put the plugin under the
   same compile-time gravity as the bridge.

The on-wire types themselves are small (4 fields each for the two
KEP-4412 messages, 4 fields for the bridge response). They change
rarely; the KEP-4412 v1 contract is GA-stable and `bridge/dataplane`'s
Response is pinned by [ADR-0013](0013-return-robot-basic-auth-credentials.md).

## Decision

The plugin defines all of its wire types locally as plain structs with
explicit JSON tags. Specifically:

- `credentialProviderRequest`, `credentialProviderResponse`,
  `authConfig` in [plugin/main.go](../../plugin/main.go) cover the
  KEP-4412 protocol.
- `bridgeResponse` in
  [plugin/bridge_client.go](../../plugin/bridge_client.go) covers the
  bridge HTTPS response.
- The bridge request body is an inline anonymous struct (`{Image
  string}`) marshaled per call — not a named type — because the
  plugin only ever produces it, never consumes it.
- `credentialProviderAPIVersion`, `requestKind`, `responseKind`,
  `cacheKeyTypeImage`, and `credentialsPath` are local string
  constants.
- `cacheDuration` is serialised as the Go duration string
  (`time.Duration.String()`) which matches `metav1.Duration`'s
  `MarshalJSON` output on the wire.
- The handler's `cache_key_type` value is passed through verbatim
  from the bridge's response field; the plugin does not validate it
  against a closed enum.

Each duplicated struct carries a comment pointing at the upstream or
in-repo source of truth, so a contributor changing the source side is
guided to update the plugin side. The comments are short on purpose:
the ADR carries the rationale.

## Consequences

- **`go list -deps ./plugin/...` returns zero `k8s.io` and zero
  `sigs.k8s.io` packages.** Verified at the time of writing; should be
  re-verified on every plugin change. Adding any such import is a
  breaking change to this ADR's intent.
- **Plugin binary stays small.** ~6 MB static at the time of writing
  (commit after Phase 4). The chart's distroless image is therefore
  thin and fast to pull onto every node.
- **Two wire formats to keep in sync.** Mitigated by:
  - The wire formats are small (≤4 fields each).
  - The bridge side has its own struct + handler tests that pin the
    JSON shape ([bridge/dataplane/handler_test.go](../../bridge/dataplane/handler_test.go)).
  - The plugin's `bridge_client_test.go` covers the wire deserialise.
  - Phase 6 e2e exercises the round trip end-to-end against real
    kubelet + containerd.
- **No upstream `k8s.io/kubelet` version bumps will move the wire
  contract under us.** Future KEP-4412 evolutions arrive only when a
  human reads the new spec and updates the plugin's local types.
- **Contributor friction.** Anyone touching the plugin will see the
  duplicated types and wonder why. This ADR + the in-file
  cross-reference comment are the standing answer. A `golangci-lint`
  rule forbidding imports of `k8s.io/kubelet` or `bridge/dataplane`
  from `./plugin/...` would mechanise the rule; left as a Phase 6
  hardening task rather than blocking on it now.

## Alternatives considered

- **Import `k8s.io/kubelet/pkg/apis/credentialprovider/v1`.** Rejected
  for the `cacheKeyType: "ServiceAccount"` enum mismatch and the
  transitive dep weight. Either alone would be tolerable; together
  they tip the balance toward duplication.
- **Vendor only `types.go` from upstream into the plugin.** Removes
  the transitive deps, keeps the named-type benefit. Rejected because
  vendoring inherits the version-mismatch risk on the enum, and Go's
  toolchain treats vendored files exactly like our own — there is no
  remaining benefit beyond the names matching upstream identifiers,
  which we did not need.
- **Lift `bridge/dataplane.Response` into a new shared
  `internal/wire` package the plugin can import.** Solves the
  transitive-dep problem for the bridge↔plugin contract. Rejected
  for two reasons. First, it does nothing about the kubelet-side
  protocol — we'd still duplicate those types, defeating the
  consistency goal. Second, it would create a new compile-time
  coupling between the bridge binary and the plugin binary; today
  they can be released independently and a future bridge version
  bump cannot accidentally tighten or loosen the plugin's wire
  contract.
- **Generate types from a shared protobuf or OpenAPI schema.**
  Disproportionate tooling cost for two messages with ≤4 fields each,
  and the kubelet-side schema is already authoritatively defined
  upstream — we'd be re-encoding it in our own IDL.
- **Skip type definitions entirely; marshal raw `map[string]any`.**
  Saves the duplication but loses type-checked JSON tag matching at
  compile time. Wire-format drift would only surface in tests or in
  production. Rejected for the loss of compile-time safety on
  something exec'd inside the image-pull critical path.

## See also

- [plugin/main.go](../../plugin/main.go) — `credentialProviderRequest`
  / `credentialProviderResponse` / `authConfig` definitions.
- [plugin/bridge_client.go](../../plugin/bridge_client.go) —
  `bridgeResponse` and `credentialsPath`.
- [bridge/dataplane/handler.go](../../bridge/dataplane/handler.go) —
  source of truth for the bridge wire contract.
- KEP-4412 — source of truth for the kubelet wire contract.
- [ADR-0002](0002-bridge-control-plane-data-plane-split.md) — the
  package-isolation discipline this ADR extends to the plugin.
- [ADR-0013](0013-return-robot-basic-auth-credentials.md) — pins the
  bridge response's semantics (Basic Auth, not JWT), the reason
  `bridgeResponse` is stable.
