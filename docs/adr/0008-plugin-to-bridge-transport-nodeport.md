# 8. Plugin-to-bridge transport: NodePort

## Status

Accepted

## Context

The kubelet credential-provider plugin is a binary the kubelet executes directly on the host (KEP-4412, Kubernetes 1.34 beta). It runs in the node's host network namespace, not inside a pod, so it cannot resolve cluster-internal DNS by default. The plugin nonetheless must reach the bridge's Data Plane; a ClusterIP Service is not reachable from the host's resolver without help.

Three viable transports:

1. **NodePort Service** on the bridge. Plugin hits `127.0.0.1:<nodePort>` (kube-proxy installs the iptables rule on every node).
2. **API-server proxy** via the kubelet's kubeconfig. Plugin reads `/var/lib/kubelet/kubeconfig` (or distro-specific path) and proxies through `kube-apiserver` to the bridge Service.
3. **hostNetwork bridge DaemonSet.** Bridge runs on every node in host network; plugin hits `127.0.0.1` on a fixed port.

## Decision

NodePort is the default. The Helm chart creates a Service of type `NodePort` and configures the plugin's `HARBOR_BRIDGE_ENDPOINT` to `https://127.0.0.1:<nodePort>`.

mTLS between plugin and bridge is supported as a Helm value (off by default, on for production) — closes the "node-local port is reachable by other workloads on the node" gap. The plugin's client certificate is provisioned by the chart and mounted alongside the plugin binary.

## Consequences

- Works on any Kubernetes distribution (kind, k3s, EKS, GKE, AKS). No assumptions about kubelet kubeconfig layout.
- Every node has a kube-proxy iptables rule sending the NodePort to the bridge Service. A workload that escapes pod network isolation could attempt to talk to the bridge directly. mTLS closes this; the bridge requires a client certificate.
- The bridge Deployment runs with a small replica count (typically 2 for HA) instead of one-per-node. Footprint stays small even on large clusters.
- We allocate a NodePort. The chart accepts a fixed port (default `31443`) or `nil` for dynamic allocation.

## Alternatives considered

- **API-server proxy.** Rejected: kubeconfig path varies by distro (`/var/lib/kubelet/kubeconfig`, `/var/lib/rancher/k3s/agent/kubelet.kubeconfig`, etc.); the plugin would need distro-aware logic. Also routes every image pull request through the apiserver, adding load and a failure coupling.
- **hostNetwork DaemonSet.** Rejected: places the bridge's robot password in the memory of every node, multiplying secret blast radius by node count. Loses the cluster-scoped admin-secret isolation that a Deployment provides.
