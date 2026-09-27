## Chart value migrations

### Unreleased: HarborAccess tokenTTL syntax, required spec

| Change | What to do |
| --- | --- |
| `spec.tokenTTL` must be a Go duration (units `h`, `m`, `s`, `ms`, `us`, `ns`, e.g. `30m`, `1h`, `1h30m`); forms such as `1d`, `3 hours` or `PT1H` are rejected | Older CRDs admitted those forms, and a single such object stopped the bridge from reconciling any HarborAccess and from serving any credential request. The bridge now reports such an object as `Ready=False`, `reason=InvalidSpec`, leaves its robot unchanged, refuses credential requests for it (`403`, audit `reason=invalid_harboraccess_spec`) and serves all others. List the values with `kubectl get harboraccess -A -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,TTL:.spec.tokenTTL` and rewrite every value that is not a Go duration, e.g. `1d` as `24h`. Helm does not upgrade CRDs from `crds/`: apply the new CRD with `kubectl apply -f charts/harbor-bridge/crds/`. |
| A HarborAccess must have a `spec`; creating one without it, or removing it, is rejected | The bridge never served such objects. It now reports existing ones as `Ready=False`, `reason=InvalidSpec` ("spec is missing") instead of `IssuerMismatch`; add a spec or delete them (deleting also revokes a robot left from before the spec was removed). They stay deletable after the CRD update. |

### Unreleased: Harbor client hardening

| Change | What to do |
| --- | --- |
| The bridge no longer follows HTTP redirects from Harbor. A followed redirect re-sent the Harbor admin credentials (on 307/308 also the request body) to a redirect target on the same host or a subdomain of it, even over plain http | Nothing, unless reconciles fail with `refusing to follow a redirect to …`. Then set `harbor.url` (`BRIDGE_HARBOR_URL`) to the https scheme and host, plus any path prefix in front of `/api/v2.0`, at which Harbor's API answers without a redirect. The error shows the full request URL: keep only its origin and path prefix. If the redirect goes to `http://`, fix the proxy rather than switching the bridge to http. |
| `harbor.url` (`BRIDGE_HARBOR_URL`) and `bridge.oidcIssuer` (`BRIDGE_OIDC_ISSUER`) must not contain `user:password@`; the bridge refuses to start | Remove it. It never took effect: the bridge authenticates to Harbor only with `harbor.adminCredsSecret`, and an issuer with credentials matches no token. The password was written to the startup log. |
| All three URL settings (`harbor.url`, `bridge.oidcIssuer`, `bridge.oidcJWKSURL`) refuse an `@` after the host part; the bridge refuses to start | Nothing, unless `bridge.oidcJWKSURL` carries credentials whose password contains `/`, `?` or `#`: percent-encode those characters (`%2F`, `%3F`, `%23`). Unencoded, they ended the host part early, so the credentials never reached the JWKS endpoint. Write an `@` that belongs to a path, query or fragment as `%40`. |
| A `harbor.robotNamePrefix` (`BRIDGE_HARBOR_ROBOT_PREFIX`) that does not match Harbor's `robot_name_prefix` now stops the bridge from acting on Harbor's robots: HarborAccess objects report `HarborError` with a message that points at the prefix, deletions wait with `DeletionBlocked`, and the janitor deletes none of the bridge's robots. Before, deleting a HarborAccess released the finalizer and left its robot alive with a valid password | Nothing if the prefixes match. Otherwise set `harbor.robotNamePrefix` to Harbor's `robot_name_prefix`. The janitor then also deletes robots that earlier deletions left behind while the prefixes differed. An empty Harbor `robot_name_prefix` cannot be matched: an empty `harbor.robotNamePrefix` selects `robot$`, so a bridge against such a Harbor, which worked before, now fails with this error. |
| A HarborAccess whose `serviceAccountRef` namespace or name contains consecutive hyphens (`--`, e.g. a punycode `xn--…` namespace) reports `Ready=False`, `reason=InvalidSpec` once, instead of `HarborError` with retries that never end | Nothing changes for these workloads: Harbor refuses robot names with doubled separators, so they never got a robot. Use a ServiceAccount whose namespace and name have no `--`. |
| `clusterName` (`BRIDGE_CLUSTER_NAME`) must not contain consecutive hyphens; the bridge refuses to start | Harbor refused every robot of such a cluster, so no HarborAccess could work. Choose a cluster name without `--`. |

### Unreleased: configurable plugin provider name (ADR-0029)

No action is needed for existing installs that keep
`plugin.hostBinaryDir`, `plugin.install.binDir`,
`plugin.install.configFile` and `plugin.install.stateDir` out of
`plugin.hostConfigDir` (the defaults do; otherwise see the directory row
below), whose `plugin.hostConfigDir` path has no symlink in it (the
default has none; otherwise see the symlink row), and, in merge mode,
whose node config kubelet can start with:
with the default `plugin.providerName` the chart renders the same
manifests, every node file keeps its path, and the upgrade restarts no
kubelet. This includes `helm upgrade --reuse-values` from 0.10.0: the
reused values lack `plugin.providerName`, and a missing name renders the
default.

| Change | What to do |
| --- | --- |
| New `plugin.providerName` (default `harbor-bridge-plugin`, a DNS label) names the kubelet provider entry and the plugin binary; a non-default name also names the install's CA, mTLS and state files | Nothing for a single install. To run a second bridge with its own chart-managed plugin, upgrade every existing release first, chart and plugin image (next row), and wait until `kubectl -n <plugin namespace> rollout status daemonset/<fullname>-plugin` has finished for each: only then do all nodes run the ADR-0029 installer. Then install the new one under a release name of its own (it names the cluster-scoped audience RBAC and trust-manager Bundle, the mTLS CN and the default `bridge.instance`; with `fullnameOverride`, set `bridge.instance` explicitly), in a namespace of its own (the bridge's leader-election Lease has a fixed name per namespace, and its janitor sweeps the robot Secrets of its namespace), with its own `plugin.providerName`, `service.nodePort`, `plugin.audience`, non-overlapping `plugin.matchImages` and, if you use one, its own `plugin.namespace` (or a shared one created outside Helm, with `plugin.createNamespace=false` on every release), and give every release a `bridge.harborAccessSelector` (README, "Several installs per cluster"). Never roll a release back to a chart or plugin image before ADR-0029 while another release's plugin is on the nodes. The name must be a string: quote a name such as `"123"`, or use `--set-string`. Changing the name of an existing install leaves its old entry and files on the nodes; remove them as described under "Uninstalling". |
| For a non-default `plugin.providerName` the chart publishes the rendered provider config under the ConfigMap key `credential-provider-config.v2.yaml`, mounted at `/config-v2`, which installers before ADR-0029 never read | Give every release a plugin image with ADR-0029: `plugin.image.tag` follows the chart unless you set it, `plugin.image.digest` never does. An older plugin image with a non-default name fails at reading the rendered config, before it writes anything on the node, and its pods stay in `Init:CrashLoopBackOff`. With the default name an older plugin image keeps working alone, but in patch and none mode rewrites the shared config with only its own entry, so it must not run next to another install. |
| Every pass writes a record of its entry next to the plugin binary (`<bin dir>/<providerName>.entry`, `harbor-bridge-plugin.entry` for the default name), which also names the chart-owned config it writes into. Patch and none mode keep other installs' entries in the chart-owned provider config when that install's binary and record, holding exactly that entry, are in `plugin.hostBinaryDir`, instead of overwriting them; everything else in the file is still dropped. Merge mode does the same when kubelet's config is a chart-owned config (its own, or one a record in kubelet's bin dir names) | Nothing. While no other install's entry is in the file, it is written as before. Keep `plugin.hostBinaryDir` for this chart's plugins only. |
| In a cloud's config, merge mode refuses to write the config or restart kubelet when kubelet would exit at startup with it: a provider whose binary is missing from kubelet's bin dir or not executable, a name that occurs twice, or a name kubelet refuses | Nothing on a working node: kubelet started with that config. If the install fails with `kubelet would not start with the credential-provider config`, fix the entry or the binary it names on the node. |
| The chart and the installer refuse a `plugin.hostBinaryDir` or `plugin.install.binDir` that is `plugin.hostConfigDir`, inside it or contains it, a `plugin.install.stateDir` that is `plugin.hostConfigDir` or inside it, and a `plugin.install.configFile` inside it (compared per path segment); in merge mode the installer also refuses a discovered kubelet bin dir in that relation, and a kubelet config inside `plugin.hostConfigDir` other than the chart-owned one | Nothing with the defaults. Otherwise move one of the directories or files: the sync container of every release can write `plugin.hostConfigDir`, the installer trusts the binaries in the bin dir and its state file, and merge mode keeps the other entries of the config it merges into. A `helm upgrade` of an existing install with such a layout fails at template time. |
| Patch mode refuses to point kubelet at its own directories while the config kubelet reads holds another install's entry (in a config in reach of a `plugin.hostConfigDir`, only an entry a record in kubelet's bin dir vouches for) | Nothing for a single install. Give every release in patch mode the same `plugin.hostBinaryDir` and `plugin.hostConfigDir`. |
| The installer opens `plugin.hostConfigDir` one path component at a time and refuses a symlink in any of them: a release's `plugin.hostConfigDir` inside another release's could otherwise be replaced by a symlink that redirects the installer's root writes | Nothing with the default `plugin.hostConfigDir`. If yours goes through a symlink (for example under `/opt`, a symlink to `/var/opt` on ostree-based systems such as Fedora CoreOS), the install pass fails with `refusing to follow symlink`: set `plugin.hostConfigDir` to the resolved path. The provider entry names the CA file under that path, so the change restarts kubelet once (in patch mode the kubelet flags change too). |
| The installer takes a lock on each node (`/run/harbor-bridge-installer.lock`, and `<provider config>.lock` next to the provider config) and records a per-install `entryHash` in its state file next to `appliedHash`, which keeps its meaning | Nothing. The first pass adds `entryHash` to the state file of the previous version without a kubelet restart; a rollback of a single install finds its own `appliedHash` and does not restart kubelet either. |
| A second bridge installed by hand with `plugin.enabled=false` (the workaround so far) | Optional: switch it to the chart's DaemonSet with the same provider name. The installer adopts an existing bridge entry of that name, but replaces the hand-copied binary only if it is byte for byte the plugin of the chart's `plugin.image` (it has no record yet): copy that version onto the nodes first, or the installer refuses the file as another program's. |

### Unreleased: data-plane fixes

| Change | What to do |
| --- | --- |
| A request with a valid token that gets `503` or `500` is logged as a `credential unavailable` audit line (`reason=secret_missing`, `secret_unreadable` or `harboraccess_lookup_failed`, with source, subject, pod and node). The regular log's `robot Secret not yet available` line is gone | Match `credential unavailable` instead of `robot Secret not yet available` in log queries. |

### 0.10.0: token lifetime cap and pod binding (ADR-0028)

| Change | What to do |
| --- | --- |
| The bridge refuses ServiceAccount tokens whose lifetime (`exp - iat`) exceeds `bridge.tokenValidation.maxLifetime` (`BRIDGE_TOKEN_MAX_LIFETIME`, default `1h`) or that carry no `iat` (or one more than five minutes in the future), with `401` and the audit category `excessive_lifetime` | Nothing for kubelet: its tokens last exactly one hour. Hand-minted tokens (local development, scripts calling `/v1/credentials`) need `--duration` of at most the maximum, e.g. `kubectl create token <sa> -n <ns> --audience=<aud> --duration=1h …`. Do not set the maximum below `1h` unless your token issuer issues shorter tokens: every kubelet request would be refused. |
| The bridge refuses tokens without the `kubernetes.io` pod claim (`bridge.tokenValidation.requirePodBinding`, `BRIDGE_REQUIRE_POD_BOUND_TOKEN`, default `true`), with `401` and the audit category `not_pod_bound` | Nothing for kubelet: its tokens are bound to the pulling pod. Bind hand-minted tokens to a pod that runs as the ServiceAccount: add `--bound-object-kind=Pod --bound-object-name=<pod>` (HOW-TO-TEST.md, Phase 3). `false` accepts unbound tokens again but weakens the bridge; use it only for local development. |
| `credential denied` audit lines with `reason=invalid_token` carry a new `category` field (`expired`, `bad_signature`, `wrong_issuer`, `malformed`, `excessive_lifetime`, `not_pod_bound`, `other`); `bridge_oidc_validation_failures_total` gains the `reason` values `excessive_lifetime` and `not_pod_bound` | Nothing. Alert on them like on `wrong_issuer`: kubelet never sends such tokens. |

### 0.9.0: optional plugin namespace (ADR-0027)

| Change | What to do |
| --- | --- |
| New `plugin.namespace`, `plugin.createNamespace`, `plugin.caBundle.source` | Optional. To run the privileged DaemonSet in its own namespace, install trust-manager, set `plugin.namespace` and point `plugin.caBundle.source` at the CA that signs the bridge certificate in trust-manager's namespace. Then label the release namespace `pod-security.kubernetes.io/enforce=restricted`. With mTLS, `bridge.mTLS.clientIssuerRef.kind` must be `ClusterIssuer`. Helm recreates the plugin objects in the new namespace; kubelet is not restarted when the rendered provider config stays the same. |

### 0.8.0: Harbor over https, least-privilege RBAC

| Change | What to do |
| --- | --- |
| `harbor.url` must use https; an `http://` URL fails at template time and at bridge start | Use https, with `harbor.caSecret` (a Secret holding the CA, key `ca.crt`) for a private CA. Only if Harbor is reachable solely over plain http (e.g. in-cluster in a test cluster), set `harbor.allowInsecureHTTP: true`. |
| The bridge serves and adopts only Secrets it labelled; a Secret at a robot Secret's name that the bridge did not write and that holds another username is reported as `RobotConflict` | Nothing, unless such a Secret exists: rename or delete it. Secrets written by older bridges are labelled on their first reconcile. |
| Bridge RBAC lost unused verbs (`update` on harboraccesses, `patch` on Secrets, all but get/create/update on Leases) | Nothing. |

### 0.7.0: audit log and rate limit

| Change | What to do |
| --- | --- |
| Audit lines moved to the `audit` logger at fixed info level. The issuance line's `image` field is now `requested_image` (truncated to 512 characters), and new fields `source`, `client_cert`, `pod`, `pod_uid`, `node` were added. Denials are logged as `credential denied` with a `reason` | Update log queries that match `image=` or expect denials only at debug level. |
| New per-source limit on the credential endpoint (`bridge.rateLimit.perSource: 20`, `burst: 100`); beyond it the bridge answers `429` and counts `bridge_credential_issuances_total{result="rate_limited"}` | Nothing for normal kubelet traffic. Raise the limit if one source legitimately sends more (e.g. a proxy in front of the NodePort); `perSource: 0` disables it. |

### 0.6.0: one audience per bridge, HarborAccess selector (ADR-0026)

| Change | What to do |
| --- | --- |
| The bridge serves only `plugin.audience` (`BRIDGE_AUDIENCE`, now required). A HarborAccess whose `spec.trustPolicy.audience` differs gets `Ready=False`, `reason=AudienceMismatch`, and its robot is not created | Set every HarborAccess's audience to `plugin.audience`. With the chart nothing else changes; `make run-local` needs `BRIDGE_AUDIENCE`. |
| New `bridge.harborAccessSelector` and `bridge.instance` | Only for several bridges on one cluster. Each bridge serves the HarborAccess objects its selector matches and uses the finalizer `harbor.aetherize.io/robot-<instance>`. Bridges that share a Harbor need different `clusterName` values. |

### 0.5.5: HarborAccess project names

| Change | What to do |
| --- | --- |
| `spec.permissions[].project` must be a valid Harbor project name (`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`); wildcards such as `*` are rejected | Such names never matched a real project, except `*`, which Harbor reads as "every project". The bridge reports affected objects as `Ready=False`, `reason=InvalidSpec`. Helm does not upgrade CRDs from `crds/`: apply the new CRD with `kubectl apply -f charts/harbor-bridge/crds/`. |

### 0.4.0 and 0.5.0: lifecycle, metrics, and plugin changes (ADR-0023/0024/0025)

No action is needed for a default install. Check these if they apply:

| Change | What to do |
| --- | --- |
| `/metrics` moved from `https://<bridge>:8443/metrics` to plain HTTP on port `metrics.port` (8080) behind a new ClusterIP Service `<release>-metrics` | The chart's ServiceMonitor follows automatically. Update custom scrape configs. |
| `tls.enabled: false` now means "bring your own Secret" and requires `tls.existingSecret` (`tls.crt`, `tls.key`, `ca.crt`) | It never worked without a pre-created Secret (the bridge always serves TLS). Set `tls.existingSecret`. |
| New `plugin.enabled` (default `true`) | Set `false` to install the plugin out of band (Talos, baked images): [docs/install-external-plugin.md](docs/install-external-plugin.md). |
| New `harbor.robotNamePrefix` (default `robot$`) | Set it if your Harbor's `robot_name_prefix` is not `robot$`. |
| New `bridge.oidcJWKSURL` | Set it to `https://kubernetes.default.svc/openid/v1/jwks` when `bridge.oidcIssuer` is a cloud issuer URL (GKE, EKS, AKS with OIDC issuer). |
| HarborAccess `metadata.name` is limited to 63 characters (CRD validation) | Longer names could never be reconciled (the robot Secret could not be labelled). Recreate such objects under a shorter name. Helm does not upgrade CRDs from `crds/`: apply the new CRD yourself — `kubectl apply -f charts/harbor-bridge/crds/` (the bridge also reports such objects as `InvalidSpec` without the CRD update). |
| Robot passwords no longer rotate on a spec change; the daily rotation is announced to the data plane | Nothing to do. Existing Secrets get the new annotations on the first reconcile, without a rotation. |
| Leftover robots are revoked: robots of a previous `serviceAccountRef` and dash-named robots from 0.2.x | Nothing to do. Expect the janitor to delete such robots on its first sweep; their passwords were still valid. |
| Before `helm uninstall`, delete your HarborAccess objects | Their finalizers revoke the robots and need the running bridge. |

### `plugin.patchKubelet` → `plugin.install.mode` (pre-release, ADR-0021)

The boolean was replaced by an install-mode enum; the chart fails at
template time if `plugin.patchKubelet` is still set.

| Before | After |
| --- | --- |
| `plugin.patchKubelet: true` (default) | `plugin.install.mode: auto` (new default; resolves to `patch` on self-managed nodes and to `merge` on managed EKS/GKE/AKS nodes) — or pin `patch` explicitly |
| `plugin.patchKubelet: false` | `plugin.install.mode: none` |

Behavioural upgrades that come with the change: `helm upgrade`s that
alter `matchImages`/`audience` now restart kubelet automatically
(content-hash idempotency — the old flag-presence guard required a
manual restart), `/etc/default/kubelet` is parse-merged instead of
overwritten, and managed clouds get first-class support via `merge`.

## Migration Path

The HarborAccess CRD is a Kubernetes-native declarative interface for "this SA gets these Harbor permissions". It is NOT expected to become an upstream Harbor API directly. Upstream Harbor will define its own configuration model for OIDC Trust (likely server-native, configured via Harbor REST API or UI). Our Bridge translates between the two.

This is the same pattern as ExternalDNS (translates K8s Ingress resources to DNS provider APIs) or cert-manager (translates K8s Certificate resources to ACME or Vault APIs). The K8s-native declarative layer is the value we add, regardless of what the upstream registry's native API looks like.

When upstream Harbor implements OIDC Trust Policies (issue #17520):

1. Upgrade Harbor to a supporting version
2. Extend the Control Plane Reconciler with a translator that writes the trust policy to Harbor's new native model, alongside the existing robot management
3. Update kubelet credential provider config to point plugins at Harbor directly instead of at the Bridge Data Plane
4. Decommission Bridge Data Plane (delete the Deployment, keep the Reconciler)
5. HarborAccess CRD apiVersion likely bumps (v1alpha1 → v1alpha2 or v1) as field structure adapts to Harbor's eventual model. Use standard Kubernetes CRD versioning and conversion webhooks if breaking changes are needed.

The Reconciler stays. The CRD remains as the user-facing declarative interface. The Bridge becomes a pure translator from K8s resources to Harbor API calls. Not a rewrite.