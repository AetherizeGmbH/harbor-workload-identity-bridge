# 38. The chart's values schema rejects unknown keys

## Status

Accepted, 2026-11-08. Adds a check in front of the template checks of
0.11.5 (MIGRATION.md, "the chart refuses values the bridge cannot work
with"); no ADR is reversed.

## Context

- The chart had no `values.schema.json`. Helm merges the user's values
  into `values.yaml`, and the templates read only the keys they know. Any
  other key is ignored without a message, and the default applies. With
  `bridge.mTLS.enable: true` (for `enabled`) the chart installs without
  mTLS: the bridge Deployment gets no `BRIDGE_TLS_CLIENT_CA_FILE`
  (`templates/bridge-deployment.yaml`), and the install succeeds.
- The template checks of 0.11.5 (`templates/_helpers.tpl`,
  `harbor-bridge.validateRequiredValues`) refuse wrong values of the keys
  the chart knows, with messages that name the value and the fix. They
  cannot see a key the chart does not know.
- Go templates treat every non-empty string as true. `enabled: "false"`,
  or `--set-string plugin.enabled=false`, deploys the plugin DaemonSet.
- Helm validates the merged values (chart defaults plus user values)
  against `values.schema.json` on `helm install`, `upgrade`, `template` and
  `lint`, before it renders anything. It lists every violation with its
  path, for example `- at '/bridge/mTLS': additional properties 'enable'
  not allowed` (Helm 4.3.0, the version CI uses). A schema error stops the
  render, so a template check never runs for a value the schema refuses.
- Helm drops a `null` default and removes a key that the user sets to
  `null` when `values.yaml` has it. It keeps a `null` for a key that
  `values.yaml` lacks, and the schema then sees the key.
- `helm upgrade --reuse-values` renders the new chart with the previous
  release's values: the old chart's `values.yaml` merged with the values
  the user gave then. Helm checks them against the new chart's schema. The
  only key ever removed from `values.yaml` is `plugin.patchKubelet`
  (ADR-0021; `git log -p` of `charts/harbor-bridge/values.yaml` and of the
  older `chart/values.yaml`). The defaults of v0.0.1, v0.2.8, v0.3.0,
  v0.4.0, v0.5.0, v0.8.0, v0.10.2, v0.11.4 and v0.11.6 pass the schema;
  those before v0.5.0 hold `plugin.patchKubelet` and fail on the
  template's message for it, as before.
- Some values of the wrong type rendered, and failed later or did
  something else. An image tag of `1.0` rendered
  `harbor-workload-identity-bridge:%!s(float64=1)`. A number as an issuer
  or Secret name rendered a manifest the API server refuses. A number in
  `clusterName` or `plugin.namespace` stopped the template with a Go type
  error. Other values of another type render as the user meant them: a
  `--set-string` replica count or port, a numeric `plugin.audience`.

## Decision

1. **A draft-07 schema.** `charts/harbor-bridge/values.schema.json` uses
   JSON Schema draft-07, which Helm 3 and Helm 4 both read.
2. **Closed where the chart owns the keys.** The top level and every
   object whose keys the chart defines set `additionalProperties: false`:
   `harbor`, `tls`, `bridge`, `plugin`, `service`, `metrics` and their
   sub-objects. These stay open:
   - `global`, which Helm passes to subcharts and umbrella charts set;
   - the maps and lists the templates copy into manifests with `toYaml`:
     `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations` and
     `resources` of `bridge` and `plugin`, `bridge.affinity`,
     `bridge.extraEnv` (Kubernetes EnvVar objects), `bridge.extraEnvFrom`
     and `metrics.serviceMonitor.labels`;
   - `bridge.harborAccessSelector`, a label map whose keys the user
     chooses.

   The schema gives them their type (object or array) and nothing more.
3. **Tombstones stay allowed.** A removed key that the templates refuse
   with a message of their own keeps an entry without a type, so that
   message, which names the replacement, still shows. The only such key
   is `plugin.patchKubelet`.
4. **Types the templates accept today.** Each key gets the JSON types that
   render correctly today, and no other:
   - Boolean switches that a template uses as a condition need `true` or
     `false`: `harbor.allowInsecureHTTP`, `tls.enabled`,
     `bridge.mTLS.enabled`, `bridge.podDisruptionBudget.enabled`,
     `plugin.enabled`, `plugin.createNamespace`,
     `plugin.audienceRBAC.create`, `plugin.allowSelfMatchImages`,
     `metrics.enabled`, `metrics.serviceMonitor.enabled`.
     `bridge.forceLocalValidation` only reaches the bridge's environment,
     where a string parses too, so it also takes a string.
   - Numbers that users pass with `--set-string` or quote also take a
     string: `bridge.replicas`, `service.port`, `service.nodePort`,
     `metrics.port`, `bridge.podDisruptionBudget.maxUnavailable` (which
     also takes a percentage), `bridge.rateLimit.perSource` and `burst`,
     `plugin.defaultCacheDuration`.
   - Names that the templates only quote also take an integer:
     `plugin.audience`, `bridge.instance`, `harbor.robotNamePrefix` and
     the names and keys of `plugin.caBundle.source`.
   - Every other key takes a string, an object or an array, as its
     default does. `service.nodePort` also takes `null`, which values.yaml
     documents.
5. **The templates keep their precise messages.** Where a template check
   explains a value, the schema checks at most its type: no `pattern`, no
   `enum`, no range and no `required`. `clusterName`, `harbor.url` and the
   other required values keep their "is REQUIRED" messages, and
   `plugin.install.mode` keeps "Must be one of: auto, merge, patch, none".
   Where a template check reports a wrong type itself, the schema sets no
   type: `bridge.leaderElection`, `bridge.tokenValidation.maxLifetime` and
   `requirePodBinding`, `plugin.providerName` and
   `plugin.priorityClassName`.
6. **A description per key.** Each key has one sentence from its comment
   in `values.yaml`, for editors with a YAML language server.
7. **Tests.** `tests/test-required-values.sh` proves that a misspelt key
   (`bridge.mTLS.enable`, a top-level key, a key in `plugin.install`) and a
   boolean switch given as a string fail with the path and the key, that
   `global` and keys of the open maps render, and that
   `plugin.patchKubelet` still fails with its own message.

## Consequences

- A misspelt or unknown key fails `helm install`, `upgrade`, `template`
  and `lint` with its path and name, before anything reaches the cluster.
- **Upgrade impact.** `helm upgrade` with a values file or `--set` that
  holds a key the chart does not know now fails and names the key. So do
  `--reuse-values` and `--reset-then-reuse-values` when the release's
  stored values hold one, such as a typo from an earlier install. Such a
  key never had an effect: the fix is to delete it. With `--reuse-values`,
  `--set <key>=null` does not remove it, because Helm keeps a `null` for a
  key `values.yaml` lacks (checked on kind with Helm 4.3.0). Save the
  values with `helm get values <release> -o yaml`, delete the key there,
  and upgrade with `-f` instead of `--reuse-values`.
- Values of the wrong type that used to render fail at template time with
  the path: a boolean switch written as a string, a number as an image tag
  or as a Secret, issuer or file name, a list where the chart
  expects a map.
- Every new key in `values.yaml` needs an entry in the schema. Helm checks
  the chart's own defaults on every render, so `make chart-test` fails
  until the entry exists. A key removed from `values.yaml` leaves the
  schema too, or stays there as a tombstone when a template refuses it
  (decision 3).
- Nothing checks that every key in the schema is still in `values.yaml`.
  A key left behind in the schema is accepted and ignored, as every key
  was before.
- The tests match Helm 4.3.0's wording of schema errors. Another Helm
  version may word them differently.

## Alternatives considered

- **No schema.** The typo case stays silent: a misspelt security switch
  such as `bridge.mTLS.enable` installs without the protection it asks
  for.
- **A schema that types the known keys and allows unknown ones.** It
  catches `enabled: "false"`, but not `enable: true`, the case that
  started this decision.
- **A template check against a list of known keys.** It would repeat
  `values.yaml` in template code, and editors could not use it to
  complete or check keys.
- **Strict types, patterns and `required`.** It would replace the precise
  template messages with generic schema messages, and refuse values that
  render correctly today, such as a numeric audience or a port passed with
  `--set-string`.
