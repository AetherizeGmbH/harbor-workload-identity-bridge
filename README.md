# Harbor Workload Identity Bridge

**Pull from Harbor with the Service Account your pod is already running as. No
imagePullSecrets, no long-lived credentials in workload namespaces, no
per-namespace token-distribution chores.**

> Status: **alpha**. The control plane is feature-complete and unit-tested.
> The end-to-end claim (ADR-0013) — that containerd accepts the credentials
> the bridge returns and completes the Harbor handshake itself — is being
> validated. See [`docs/PHASES.md`](docs/PHASES.md) for what's live and
> what's next.

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
- A plugin binary the kubelet runs per node (Phase 4, in progress).
- A Helm chart that installs all of it (Phase 5, in progress).

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

> The Helm chart (Phase 5) is not yet shipped. The commands below assume
> you have built the bridge binary from source and are running it
> locally against a kind cluster + a Harbor instance. The chart will
> collapse most of this.

```bash
# 1. Install the CRD.
kubectl apply -f config/crd/bases/harbor.aetherize.io_harboraccesses.yaml

# 2. Create the bridge namespace and the admin-credentials Secret
#    (must hold username + password keys; the bridge mounts this).
kubectl create namespace harbor-bridge-system
kubectl create secret generic harbor-admin \
  --from-literal=username=admin \
  --from-literal=password=Harbor12345 \
  -n harbor-bridge-system

# 3. Apply a HarborAccess CR. (Substitute your own SA name, audience,
#    projects.)
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
    issuer: https://kubernetes.default.svc
    audience: harbor.example.com
  permissions:
    - project: production
      action: pull
  tokenTTL: 1h
YAML

# 4. Expose the cluster's apiserver locally so the bridge can fetch the
#    JWKS off-cluster. Leave this running in a second terminal.
make proxy   # = kubectl proxy --port=8001

# 5. Run the bridge locally (one-time self-signed cert is auto-generated
#    in /tmp/bridge-tls). The Helm chart will replace steps 4–5.
BRIDGE_CLUSTER_NAME=dev \
BRIDGE_NAMESPACE=harbor-bridge-system \
BRIDGE_OIDC_ISSUER=$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer) \
BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks \
BRIDGE_HARBOR_URL=https://your-harbor.example.com \
BRIDGE_HARBOR_ADMIN_DIR=/path/to/admin-creds-dir \
make run-local
```

Within a few seconds you should see a Harbor robot named
`bridge-dev-flux-system-source-controller` appear in your Harbor admin
UI, and a `robot-harbor-bridge-system-flux-access` Secret in the bridge
namespace.

## Architecture and decisions

- [`docs/PHASES.md`](docs/PHASES.md) — what is done, what is next, what
  is intentionally out of scope. Written to survive context compaction;
  read this first when resuming work.
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

## Status and roadmap

| Phase | What | State |
| --- | --- | --- |
| 1 | Scaffolding, CRD types, ADRs 0001–0008 | ✅ Complete |
| 2 | Control plane: config, Harbor client, reconciler, janitor, ADRs 0009–0012 | ✅ Complete |
| 3 | Data plane: OIDC validator, HTTP handler, HTTPS server, metrics, cmd/main.go, ADR-0013 pivot | ✅ Complete |
| 4 | Plugin binary (KEP-4412 stdin/stdout protocol) | ⏳ Next |
| 5 | Helm chart (bridge + plugin DaemonSet + cert-manager + kubelet config) | ⏳ Pending |
| 6 | End-to-end tests across two clusters + one Harbor, full docs, v0.1.0 tag | ⏳ Pending |

## Contributing

This is single-maintainer alpha and not yet open to contributions. File
an issue if you've found a bug or want to discuss the design. Any
non-trivial change ships with an ADR.

## License

Apache 2.0 (per `SPDX-License-Identifier` headers on every source file).
