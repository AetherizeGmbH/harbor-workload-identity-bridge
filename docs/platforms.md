# Node platforms

What the plugin installer (ADR-0021) does on each platform, and what is
verified. "Verified" means: covered by the e2e harness against a real cluster.

| Platform | Chart settings | What happens on each node | Verified |
|---|---|---|---|
| kind, kubeadm | defaults (`install.mode: auto`) | no credential-provider flags on kubelet → **patch**: binary and config into the chart's dirs, flags added to `KUBELET_EXTRA_ARGS` in `/etc/default/kubelet`, kubelet restarted and verified | yes (CI e2e) |
| GKE Standard | defaults | kubelet already runs GKE's `auth-provider-gcp` → **merge** into that config (YAML), binary into its bin dir | harness ready (`make e2e-gke`), never run |
| EKS (AL2023) | defaults | kubelet already runs `ecr-credential-provider` → **merge** into `/etc/eks/image-credential-provider/config.json` (stays JSON) | merge unit-tested, not run |
| AKS | defaults | kubelet already runs `acr-credential-provider` → **merge** into its config | merge unit-tested, not run |
| Talos | `plugin.enabled: false` | nothing — the plugin comes from a system extension, the entry from the machine config | documented ([install-external-plugin.md](install-external-plugin.md)) |
| k3s, RKE2 | `install.mode: none`, or `plugin.enabled: false` | `none`: files only; pass the two flags via the distribution's kubelet args (`--kubelet-arg` / `kubelet-arg:`) | documented |
| baked node images | `plugin.enabled: false` | nothing | documented |
| GKE Autopilot | — | not supported: no privileged DaemonSets, no custom credential-provider binaries | — |
| Bottlerocket | — | not supported: read-only root, only built-in credential providers | — |

## Requirements on every platform

- Kubernetes **1.34+** (KEP-4412: kubelet passes a ServiceAccount token to the
  credential provider; beta and on by default from 1.34).
- `ServiceAccountNodeAudienceRestriction` (on by default since 1.32) — the chart
  creates the RBAC that lets kubelets request tokens for `plugin.audience`.
- Each node must reach the bridge. Default: the bridge NodePort on the node's
  own loopback (`https://127.0.0.1:31443`). On dataplanes that do not route
  loopback NodePorts set `plugin.bridgeEndpoint: "https://$(NODE_IP):31443"`. The
  chart then has the plugin verify the bridge certificate against the bridge
  Service's DNS name, because the node IP is not in the certificate.
- The bridge and plugin images must come from a registry outside
  `plugin.matchImages` (the chart refuses otherwise; ADR-0021).

## Managed clouds: what to expect

**Restarts.** In auto/merge/patch mode the installer restarts kubelet once per
node when (and only when) the effective credential-provider config changes,
then waits until kubelet is stably active before it records success. Running
containers are not affected. A node whose kubelet does not come back fails the
DaemonSet pod on that node (visible in `kubectl logs ds/…-plugin -c install`);
it does not continue silently.

**Node replacement and reboots.** New or reimaged nodes run the DaemonSet and
converge the same way. If a platform resets the provider config at boot (GKE COS
keeps `/etc` stateless), the next DaemonSet pod start merges again and restarts
kubelet once.

**GKE.** The e2e harness (`test/e2e-gke`, ADR-0022) creates a zonal spot cluster
with Dataplane V2, Workload Identity (pods cannot read the node's credentials),
legacy metadata endpoints off, and the control plane reachable only from the
operator's IP. It is billed and has never been run. First-run findings go into
ADR-0022.

**EKS / AKS.** A harness would reuse the shared modules in `test/e2e/modules`
(token-auth kubeconfig, LoadBalancer Harbor, pull jobs with diagnostics, the
HarborAccess scenario) with a cloud-specific cluster module, like the GKE one.
Points to confirm on the first run:

- the node's credential-provider config path and format (the installer reads the
  live kubelet flags, so it does not depend on a hard-coded path);
- that kubelet is started by systemd with PID 1 as parent (the installer refuses
  any other `kubelet` process — a pod can name its process `kubelet`);
- that `127.0.0.1:<nodePort>` routes to the bridge (VPC CNI / Azure CNI).
