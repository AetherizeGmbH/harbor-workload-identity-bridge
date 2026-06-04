# E2E Manual Setup Runbook (2026-06-03/04)

Written as a snapshot of the manual steps we did to install the chart
end-to-end on `tofu-dev` and the bugs they expose. **The point of this
document is to make the work automatable in
[test/e2e/](../test/e2e/)** — every section here corresponds to a step
the tofu test must perform.

If we compact mid-automation, this is the source of truth for what
needs to happen.

## Cluster topology (target)

Single kind cluster, kube-proxy replaced by Cilium (BPF), Traefik as
ingress, Harbor in-cluster, cert-manager installed. No GCP, no MHS,
no multi-cluster.

```
laptop ──────────────────────────────────────────────────────────────┐
        :8443 (kind extra_port_mapping → 30843 → traefik:8443)       │
                                                                     │
docker network: tofu-dev (172.18.0.0/16)                             │
┌──────────────────────────────────────────────────────────────────┐ │
│ kind nodes: tofu-dev-{control-plane,worker,worker2,worker3}      │ │
│  - cilium (kube-proxy replacement, bpf-lb-sock: false)           │ │
│  - traefik NodePort :30843 (TLS) / :30880 (HTTP)                 │ │
│  - harbor in `harbor` ns, ingress harbor.dev.127.0.0.1.nip.io    │ │
│  - cert-manager in `cert-manager` ns                             │ │
│  - kubelet: KEP-4412 feature gates ON, no -bin-dir flag at boot  │ │
│    (chart's DaemonSet wires + restarts kubelet at install time)  │ │
└──────────────────────────────────────────────────────────────────┘ │
                                                                     │
harbor-bridge-system: bridge Deployment + plugin DaemonSet (chart)   │
test-pull:            image-puller SA + HarborAccess CR + pod        │
```

## Sharp edges (each becomes an automation requirement)

### 1. CoreDNS template targets a wrong service in this cluster

Stock CoreDNS Corefile in the test-cluster module:

```
template IN ANY 127.0.0.1.nip.io {
  answer "{{ .Name }} 60 IN CNAME ingress-nginx-controller.kube-system.svc.cluster.local"
}
```

There is no `ingress-nginx-controller` service in this cluster — the
ingress controller is Traefik at `traefik.traefik.svc.cluster.local`.

**Fix applied:** patched the CoreDNS ConfigMap to CNAME at Traefik
instead, then `rollout restart deploy/coredns`.

**Automation:** the `test-coredns-cm` module (or a copy of it) needs to
write the Traefik-pointing template. Or: have the chart's plugin
DaemonSet accept the FQDN via values and the chart caller passes the
right ingress host. Most likely: bake the right template in the
copied test-coredns-cm module since we control it.

### 2. /etc/hosts on every kind node maps FQDN to node IP

Containerd runs in the kind node's **host network namespace**, NOT
inside a pod. CoreDNS therefore does not apply to containerd
resolution. Additionally `bpf-lb-sock: false` in Cilium means
ClusterIPs aren't reachable from host-net-ns either.

So containerd's DNS lookup of `harbor.dev.127.0.0.1.nip.io` goes to
the node's /etc/resolv.conf (public DNS), which resolves nip.io to
127.0.0.1 — dead-end inside the kind node.

**Fix applied:** added `<node-IP> harbor.dev.127.0.0.1.nip.io` to
/etc/hosts on each kind node. The IP can be that node's own docker
IP since Traefik NodePort is reachable on every node
(`externalTrafficPolicy: Cluster`).

**Automation:** add the /etc/hosts entry in the `kind-cluster` module's
node-bootstrap hook, OR — better — at the chart's plugin DaemonSet
init step (write to `/host/etc/hosts` via the bind mount). The latter
generalises to any kind cluster.

### 3. Image refs MUST use `:30843` (Traefik NodePort), not `:8443`

Containerd's `hosts.toml` mirror mechanism rewrites the Host header to
the mirror's URL, breaking Traefik's host-based ingress routing. So we
can't transparently redirect `:8443` → `:30843`.

Image refs in pods + chart matchImages therefore include `:30843`
explicitly:

- pod: `harbor.dev.127.0.0.1.nip.io:30843/your-project/alpine:test3`
- chart: `plugin.matchImages: ["harbor.dev.127.0.0.1.nip.io:30843/*"]`

**Automation:** the harbor-bridge-install tofu module's `matchImages`
input takes the Traefik-NodePort form. Default in the test set.

### 4. containerd needs `skip_verify` for Harbor's self-signed cert

Harbor is signed by cert-manager's self-signed ClusterIssuer in this
test setup. Containerd's default trust store doesn't trust that CA.

**Fix applied** on each kind node:

```
mkdir -p /etc/containerd/certs.d/harbor.dev.127.0.0.1.nip.io:30843
cat > /etc/containerd/certs.d/harbor.dev.127.0.0.1.nip.io:30843/hosts.toml <<EOF
server = "https://harbor.dev.127.0.0.1.nip.io:30843"
[host."https://harbor.dev.127.0.0.1.nip.io:30843"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
EOF
echo "[plugins.\"io.containerd.grpc.v1.cri\".registry]" >> /etc/containerd/config.toml
echo "  config_path = \"/etc/containerd/certs.d\"" >> /etc/containerd/config.toml
systemctl restart containerd
```

**Automation:** kind's `containerd_config_patches` block in the
`kind-cluster` module. Per user request, do this in the module
directly:

```hcl
kind_config {
  containerd_config_patches = [
    <<-TOML
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
    TOML
  ]
}
```

…then either: also write the `hosts.toml` via extra_mounts of a host-
side file, OR have the chart's plugin DaemonSet write it. The hosts
file approach in the module is cleaner.

### 5. Plugin binary + config MUST exist before kubelet sets the flags

Kubelet's `kuberuntime_manager.go:301` startup check:

```
"Failed to register CRI auth plugins"
err="plugin binary directory /etc/kubernetes/credential-provider did not exist"
```

…or:

```
err="unable to access path '.../credential-provider-config.yaml': no such file or directory"
```

Kubelet fails to start if either path doesn't exist when it boots.

**Resolution:** chart's plugin DaemonSet writes files THEN patches
kubelet flags THEN restarts kubelet (via `nsenter` into PID 1). Test
cluster module does NOT pre-bake kubelet flags — that would catch-22.

This matches `plugin.patchKubelet: true` default in chart values.yaml.

### 6. Helm upgrade doesn't restart kubelet if config CONTENT changes

The chart's idempotency guard only checks for `image-credential-provider-bin-dir`
in `/etc/default/kubelet`. If you `helm upgrade` and change
`matchImages` / `audience` / TLS, the on-node file content updates
but kubelet keeps the OLD providers (kubelet reads the config once at
boot; no hot-reload — DynamicKubeletConfig was removed in 1.26).

**Workaround we used:**

```bash
for node in $(kubectl get nodes -o name); do
  docker exec ${node##*/} systemctl restart kubelet
done
```

**TODO (chart):** make idempotency content-aware (hash the config and
compare with what's about to be written). Then the e2e module
`harbor-bridge-install` can `helm install` / `upgrade` without the
follow-up restart loop.

### 7. Harbor needs the project pre-created OR HarborAccess will fail

If `spec.permissions[].project` references a non-existent Harbor
project, the bridge logs `HarborError: create robot: NOT_FOUND: project
... not found` and the CR stays Ready=False. There's no auto-create.

**Fix applied:** `curl -X POST /api/v2.0/projects` with admin creds.

**Automation:** add a `harbor-project` resource (helm chart's
`extra_objects` OR a direct API call from tofu via `null_resource` +
`local-exec`, OR use a published Harbor terraform provider). Probably
the cleanest: the e2e test uses the existing `harbor` tofu module
which should already know how to create projects.

### 8. Image MUST be present in Harbor before the pull pod runs

Harbor doesn't auto-pull from upstream registries. We pushed
`alpine:3.20` → `harbor.dev.127.0.0.1.nip.io:8443/your-project/alpine:test3`
manually via `crane copy --insecure`.

**Automation:** e2e test seeds the test image during setup. crane (or
oras/skopeo) called from a `null_resource` after Harbor is healthy.
The push uses the laptop-side URL `:8443` (kind port mapping); the
content is then accessible at the in-cluster `:30843` URL because
Harbor stores blobs once.

## The actual silent-abort bug (reproduced 2026-06-03)

Once steps 1–8 are correct and the chart is fully installed:

- Plugin DaemonSet has all files on every node
- Kubelet has flags + restarted (`plugins.go:55 "Registered credential provider"`)
- HarborAccess CR Ready=True, robot in Harbor, Secret in bridge ns
- Image present at `harbor.dev.127.0.0.1.nip.io:30843/your-project/alpine:test3`
- Bridge endpoint reachable, bridge credentials verified via direct
  curl + on-node manual `harbor-bridge-plugin` invocation

Apply a pod referencing the image. Result:

```
plugins.go:75  "Generating per pod credential provider" provider="harbor-bridge-plugin"
... 115 microseconds later ...
kuberuntime_image.go:39  "Pulling image without credentials"
```

The plugin binary is **never executed**. Confirmed by wrapper script
around the binary that logs to `/tmp/plugin-invoked.log` — file
remains absent. Confirmed across:

- `tokenAttributes` set vs stripped (rules out
  `KubeletServiceAccountTokenForCredentialProviders` feature gate)
- both 2026-05-31 and 2026-06-03 cluster builds
- v=4 kubelet logging (no error between Generating and the next
  Pulling line)
- `kubectl create token --bound-object-kind=Pod --audience=harbor-bridge`
  succeeds, so SA token projection is allowed by the apiserver

Cluster details:

- kubelet v1.35.0 (kindest/node)
- feature gates: `KubeletServiceAccountTokenForCredentialProviders=true`,
  `ServiceAccountNodeAudienceRestriction=true`
- Cilium kube-proxy replacement, `bpf-lb-sock: false`
- CredentialProviderConfig apiVersion: `kubelet.config.k8s.io/v1`

The wrapper-never-called fact rules out anything inside the plugin
binary. The 115-microsecond gap rules out exec-then-empty-response.
Something inside kubelet's `pluginProvider.Provide()` between the
`klog.V(5)` log and the `runPlugin()` call short-circuits.

Candidate causes still on the table:
- Some persistent or in-process cache returning empty from a prior
  failed lookup (but kubelet was restarted multiple times and cache
  is documented as in-memory)
- A KEP-4412 ServiceAccount-cache-type code path that requires fields
  we haven't set (but the symptom persists with tokenAttributes
  removed entirely — so unlikely)
- kind-specific kubelet build issue

**Upstream issue draft** lives in [docs/UPSTREAM-ISSUE.md](UPSTREAM-ISSUE.md)
once we have one (TODO).

## Reproduction recipe

Self-contained reproduction so an upstream maintainer can run it
without our chart:

```bash
# Standard kind cluster on 1.35 with the relevant gates
cat <<'EOF' > /tmp/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  KubeletServiceAccountTokenForCredentialProviders: true
nodes:
  - role: control-plane
  - role: worker
EOF
kind create cluster --image kindest/node:v1.35.0 --config /tmp/kind-config.yaml --name kep4412

# Place a minimal exec-print-stdin "provider"
docker exec kep4412-worker mkdir -p /etc/kubernetes/credential-provider /etc/kubernetes/credential-provider-config
docker cp - kep4412-worker:/etc/kubernetes/credential-provider/printer <<'BIN'
#!/bin/sh
echo "[$(date +%H:%M:%S.%N)] PROVIDER CALLED" >> /tmp/provider.log
cat >> /tmp/provider.log
printf '{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderResponse","cacheKeyType":"Image","cacheDuration":"1m","auth":{"*":{"username":"x","password":"x"}}}'
BIN
docker exec kep4412-worker chmod +x /etc/kubernetes/credential-provider/printer

docker exec kep4412-worker sh -c 'cat > /etc/kubernetes/credential-provider-config/cfg.yaml' <<'EOF'
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: printer
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages: ["fake.example.com/*"]
    defaultCacheDuration: "1m"
EOF

docker exec kep4412-worker sh -c '
  echo "KUBELET_EXTRA_ARGS=--v=4 --image-credential-provider-bin-dir=/etc/kubernetes/credential-provider --image-credential-provider-config=/etc/kubernetes/credential-provider-config/cfg.yaml" > /etc/default/kubelet
  systemctl daemon-reload
  systemctl restart kubelet
'

# Apply a pod whose image matches matchImages
kubectl run repro --image=fake.example.com/foo:bar --restart=Never --command -- sleep 60

# Expected: /tmp/provider.log on the node has at least one PROVIDER CALLED line
# Observed: file does not exist; kubelet logs show "Generating per pod credential provider"
#           then "Pulling image without credentials" ~100µs later
docker exec kep4412-worker cat /tmp/provider.log 2>&1
docker exec kep4412-worker journalctl -u kubelet --no-pager | grep -E "plugins.go:75|kuberuntime_image.go:39"
```

## File-level diff summary of what we wrote during this run

Working tree files modified (both repos):

- `harbor-workload-identity-bridge/chart/templates/plugin-daemonset.yaml`
  — `hostPID: true` + nsenter patch+restart of kubelet at install
- `harbor-workload-identity-bridge/chart/values.yaml` — added
  `plugin.patchKubelet` (default true)
- `harbor-workload-identity-bridge/chart/templates/NOTES.txt` —
  branches on `patchKubelet`
- `harbor-workload-identity-bridge/chart/tests/values-e2e.yaml` —
  `matchImages: ["harbor.dev.127.0.0.1.nip.io:30843/*"]`
- `harbor-workload-identity-bridge/chart/tests/golden/{default,mtls}.yaml`
  — regenerated
- `harbor-workload-identity-bridge/README.md` — quickstart updated for
  patchKubelet=true, helm-upgrade caveat documented
- `harbor-workload-identity-bridge/docs/PHASES.md` — Phase 6 in
  progress section
- `terraform-modules/modules/test-cluster/main.tf` — `feature_gates`
  block (gates on); `image-credential-provider-*` flag attempts
  reverted
