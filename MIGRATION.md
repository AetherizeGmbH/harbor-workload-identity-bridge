## Chart value migrations

### Unreleased: optional plugin namespace (ADR-0027)

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