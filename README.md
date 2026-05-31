# Harbor Workload Identity Bridge

**Pull from Harbor with the Service Account your pod is already running as. No
imagePullSecrets, no long-lived credentials in workload namespaces, no
per-namespace token-distribution chores.**

> Status: **alpha**. Bridge, plugin, and Helm chart are feature-complete
> and verified end-to-end on a single kind cluster against a real Harbor:
> `crane pull` succeeded using the credentials the bridge returned
> ([ADR-0013](docs/adr/0013-return-robot-basic-auth-credentials.md)), and
> the plugin's `CredentialProviderResponse` carries a Basic Auth pair
> byte-equal to what `curl` receives from the bridge directly. What's
> left for v0.1.0 is the kubelet-driven fork+exec path (Phase 6 e2e
> against a kind cluster recreated with `--image-credential-provider-*`
> flags) and the SECURITY.md threat model polish. See
> [`docs/PHASES.md`](docs/PHASES.md) for the full state.

---

## The problem

Pulling from a private registry in Kubernetes still works the way it did
in 2016:

- You create an `imagePullSecret` in every namespace that needs to pull.
- The Secret holds a long-lived robot password.
- Any pod in that namespace can `kubectl exec → cat /run/secrets/...` and
  exfiltrate the credentials.
- Rotation requires touching every namespace at once.

Cloud-managed registries (ECR, GCR, ACR) sidestep this through
[KEP-4412](https://github.com/kubernetes/enhancements/issues/4412): the
kubelet exec's a credential-provider plugin per pull, the plugin
authenticates the **workload** via its Service Account token, and the
returned credentials never touch the workload's namespace.

Harbor doesn't have this yet — [goharbor/harbor#17520](https://github.com/goharbor/harbor/issues/17520)
tracks the upstream OIDC trust-policy work. Once it lands, the standard
SA-token → registry flow will work natively with Harbor. **This project
is the bridge in between.**

## What you get

- A `HarborAccess` CRD: "this Service Account in this cluster gets these
  permissions on these Harbor projects."
- A controller that materialises each CR into a persistent Harbor robot
  account, rotates its password every 24h, and tears it down when the CR
  is deleted.
- A small HTTPS server that the kubelet plugin asks for credentials per
  pull. SA token in, robot Basic Auth credentials out.
- A KEP-4412 credential-provider plugin binary, a stateless adapter
  between kubelet's stdin/stdout protocol and the bridge's HTTPS API
  ([ADR-0015](docs/adr/0015-plugin-duplicates-wire-types.md)).
- A Helm chart that installs both: bridge as a Deployment in the release
  namespace, plugin as a DaemonSet that copies the binary + kubelet
  config + bridge CA onto every node's filesystem. Required values
  fail-fast with action-oriented errors at template time.

When upstream Harbor lands #17520, you delete the HTTPS server and the
plugin; the CRD and reconciler survive as a thin declarative layer. This
is the same shape ExternalDNS and cert-manager have to their respective
backends.

## How it works

![Credential issuance flow](docs/img/request-flow.svg)

For every image pull, the kubelet runs the plugin, which calls the bridge
with the pod's SA token. The bridge validates the token's signature,
expiry, and issuer locally; finds the `HarborAccess` whose
`serviceAccountRef` and `trustPolicy.audience` match the token; reads the
robot's Basic Auth credentials from a Secret in the bridge's own
namespace; and returns them.

The kubelet then hands those credentials to containerd, which does the
**standard Harbor handshake itself** — the same `401 →
WWW-Authenticate: Bearer → POST /service/token → scoped JWT → pull`
dance that containerd does for every registry. The bridge does not
pre-mint JWTs. (See [ADR-0013](docs/adr/0013-return-robot-basic-auth-credentials.md)
for why earlier iterations got this wrong.)

## Multi-cluster, by design

Many clusters can share one Harbor. Each cluster runs its own bridge;
there is no central coordinator. Robots are name-prefixed
`bridge-<cluster-name>-<sa-namespace>-<sa-name>` so the prefix is the
ownership boundary.

![Multi-cluster topology](docs/img/multi-cluster.svg)

A defense-in-depth tag in the robot's Harbor description (`cluster=<name>`)
guards against the edge case where one cluster's name is a hyphen-prefix
of another. See [ADR-0009](docs/adr/0009-multi-cluster-topology.md) for
the full ownership model and the one operator burden it imposes (cluster
names must not be hyphen-prefixes of each other).

## Security model

Short version: the robot password is in a Kubernetes Secret in the
bridge's own namespace, not in your workload's namespace. Workloads have
no RBAC path to read it. It enters kubelet/containerd memory for the
duration of a pull and never lands on disk in the workload's pod. The
reconciler rotates it every 24h, bounding the blast radius if a node is
ever compromised.

That's materially better than `imagePullSecrets`. It is not
perfect — there's a 24h window after compromise, and containerd does
touch the password in memory. The full threat model, including what
the bridge does *not* defend against, is in [SECURITY.md](SECURITY.md).

## Quickstart

Prerequisites: a Kubernetes cluster (v1.34+ for KEP-4412 beta), a Harbor
instance with admin credentials, cert-manager installed, Helm 3+.

```bash
# 1. Pre-create the admin-creds Secret (the chart does not — your
#    Harbor admin password should never live in values.yaml).
kubectl create namespace harbor-bridge-system
kubectl create secret generic harbor-admin -n harbor-bridge-system \
  --from-literal=username=admin \
  --from-literal=password=YOUR_HARBOR_ADMIN_PASSWORD

# 2. Point cert-manager at an Issuer that signs the bridge's TLS cert.
#    Self-signed is fine for evaluation:
cat <<'YAML' | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: harbor-bridge-ca }
spec: { selfSigned: {} }
YAML

# 3. Install the chart.
helm install harbor-bridge ./chart -n harbor-bridge-system \
  --set clusterName=prod-eu-west \
  --set harbor.url=https://harbor.example.com \
  --set harbor.adminCredsSecret.name=harbor-admin \
  --set plugin.audience=harbor-bridge-prod-eu-west \
  --set 'plugin.matchImages={harbor.example.com/*}' \
  --set tls.issuerRef.name=harbor-bridge-ca

# 4. Configure each node's kubelet to use the plugin. The chart can't
#    set these flags for you; do it at node provisioning:
#      --image-credential-provider-bin-dir=/etc/kubernetes/credential-provider
#      --image-credential-provider-config=/etc/kubernetes/credential-provider-config/credential-provider-config.yaml
#    For kind, see https://kind.sigs.k8s.io/docs/user/configuration/#kubelet-extra-args.

# 5. Apply a HarborAccess CR. The audience MUST match plugin.audience above.
cat <<'YAML' | kubectl apply -f -
apiVersion: harbor.aetherize.io/v1alpha1
kind: HarborAccess
metadata:
  name: flux-access
  namespace: harbor-bridge-system
spec:
  serviceAccountRef:
    namespace: flux-system
    name: source-controller
  trustPolicy:
    issuer: https://kubernetes.default.svc.cluster.local
    audience: harbor-bridge-prod-eu-west
  permissions:
    - project: production
      action: pull
  tokenTTL: 1h
YAML
```

Within a few seconds a `bridge-prod-eu-west-flux-system-source-controller`
robot appears in Harbor's admin UI, the bridge namespace gets a
`robot-harbor-bridge-system-flux-access` Secret, and pods running as
`flux-system/source-controller` can pull from `harbor.example.com/production/*`.

**Local development** (no chart, run the bridge against your kubeconfig
via `kubectl proxy`) is documented in [HOW-TO-TEST.md](HOW-TO-TEST.md)
along with the manual plugin-driver procedure that proves the chain
end-to-end without a kubelet-config change.

## Architecture and decisions

- [`docs/PHASES.md`](docs/PHASES.md) — what is done, what is next, what
  is intentionally out of scope. Written to survive context compaction;
  read this first when resuming work.
- [`HOW-TO-TEST.md`](HOW-TO-TEST.md) — reproducible end-to-end procedure
  with local bridge, kubectl proxy, and a manual plugin-driver round-trip.
- [`docs/adr/`](docs/adr/) — every load-bearing design decision has an
  ADR. The ones most likely to surprise you:
  - [ADR-0002](docs/adr/0002-bridge-control-plane-data-plane-split.md) —
    control plane and data plane live in one binary but in separate
    packages. The data plane is deletable as a single PR when Harbor
    #17520 lands.
  - [ADR-0009](docs/adr/0009-multi-cluster-topology.md) — multi-cluster
    ownership model and the prefix-collision operator caveat.
  - [ADR-0011](docs/adr/0011-robot-password-secret-storage.md) — why
    the robot Secret lives in the bridge namespace, not the CR's
    namespace.
  - [ADR-0013](docs/adr/0013-return-robot-basic-auth-credentials.md) —
    why we return Basic Auth credentials instead of pre-minting Docker
    bearer JWTs.
  - [ADR-0014](docs/adr/0014-harbor-robot-dollar-prefix-handling.md) —
    Harbor's `robot$` prefix asymmetry between POST and GET.
  - [ADR-0015](docs/adr/0015-plugin-duplicates-wire-types.md) — why
    the plugin defines its own wire types instead of importing
    `k8s.io/kubelet` or `bridge/dataplane`. (Mechanised via
    `make verify-plugin-isolation`.)

## Status and roadmap

| Phase | What | State |
| --- | --- | --- |
| 1 | Scaffolding, CRD types, ADRs 0001–0008 | ✅ Complete |
| 2 | Control plane: config, Harbor client, reconciler, janitor, ADRs 0009–0012 | ✅ Complete |
| 3 | Data plane: OIDC validator, HTTP handler, HTTPS server, metrics, cmd/main.go, ADR-0013 pivot | ✅ Complete |
| 4 | Plugin binary (KEP-4412 stdin/stdout protocol), ADR-0015 | ✅ Complete |
| 5 | Helm chart (bridge + plugin DaemonSet + cert-manager + kubelet config) | ✅ Complete |
| 6 | Kubelet-driven e2e (kind cluster with `--image-credential-provider-*` flags) + SECURITY.md polish + v0.1.0 tag | ⏳ Next |

## Contributing

This is single-maintainer alpha and not yet open to contributions. File
an issue if you've found a bug or want to discuss the design. Any
non-trivial change ships with an ADR.

## License

Apache 2.0 (per `SPDX-License-Identifier` headers on every source file).
