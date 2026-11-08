# 37. The plugin exits non-zero when the bridge refuses

## Status

Accepted, 2026-11-08. Changes the plugin behavior that the Phase 4 build
log recorded (`docs/PHASES.md`); no earlier ADR decided it.

## Context

- On `401` or `403` from the bridge the plugin wrote an empty `auth` map
  with `cacheDuration: 0s` and exited 0 (`plugin/main.go`). Its reason
  went to stderr.
- Kubelet reads the plugin's stderr only when the plugin fails
  (`pkg/credentialprovider/plugin/plugin.go`, `ExecPlugin`: on an error
  from `cmd.Run` it returns `"%w: %s"` with the stderr text). After exit 0
  it decodes stdout and drops stderr.
- On a failed run kubelet logs the error at error level with the image
  (1.34: `Failed getting credential from external registry credential
  provider`; 1.35+: `Failed to provide credentials for image`, with the pod
  and the ServiceAccount), counts it in
  `kubelet_credential_provider_plugin_errors_total`, and returns no
  credentials for this provider. It caches nothing, so the next pull runs
  the plugin again. That is the same outcome for the pull as the empty
  response with `cacheDuration: 0s`.
- So a refused pull left no trace on the node. The pod event shows only
  Harbor's `401` for the anonymous pull. To find the reason, the user had
  to know that the bridge's audit log exists and search it on every
  replica. The usual causes are a missing HarborAccess, a wrong audience
  or a HarborAccess with an invalid spec, and each is easy to fix once it
  is known.
- The bridge's `401` and `403` bodies are short and contain no secret:
  `invalid token`, `no matching HarborAccess for the requesting service
  account and audience`, `the matching HarborAccess has an invalid spec;
  see its Ready condition`, `the HarborAccess for the requesting service
  account is being deleted`, `credential Secret ownership mismatch`
  (`bridge/dataplane/handler.go`). The plugin cuts them to 256 bytes
  (`maxBodySnippetLen`).

## Decision

1. On `401` or `403` the plugin writes nothing to stdout and exits 1. Its
   stderr line holds the status, the bridge's body and a pointer to the
   audit log: `harbor-bridge-plugin: bridge refused credentials: 403 no
   matching HarborAccess for the requesting service account and audience;
   the bridge's audit log ("credential denied") names the reason`.
2. The bridge's bodies stay as they are. The audit log keeps the detailed
   reason, for example the token validation category, which the bridge
   does not send to the caller.
3. The kind e2e proves it: `pull_pod_no_grant` pulls as `test-pull/default`,
   which no HarborAccess names, and passes only when the kubelet journal
   holds the plugin's line for that image.

## Consequences

- A user who runs `journalctl -u kubelet` or reads the node's logs sees why
  a pull got no credentials, next to the image and, from 1.35, the pod.
- `kubelet_credential_provider_plugin_errors_total` now also counts
  refusals. A workload that pulls a matched image without a HarborAccess
  raises it on its node. An alert on this counter now also fires for a
  missing grant. The counter does not tell a refusal from an unreachable
  bridge; the log line does.
- Kubelet's behavior for the pull does not change: no credentials from
  this provider, nothing cached, a new plugin run on the next pull.
- The plugin's response contract on success does not change, so the wire
  types (ADR-0015, ADR-0016) stay as they are.

## Alternatives considered

- **Keep exit 0.** The kubelet counter stays at zero for refusals, but the
  reason stays invisible on the node. Rejected: a missing or wrong
  HarborAccess is an error the user can fix in their own namespace once
  they see it, and the cost of showing it is one count in a kubelet error
  counter.
- **Send the reason in a Kubernetes Event.** The plugin has no Kubernetes
  client and must not get one (ADR-0015), and the bridge does not know the
  pod.
