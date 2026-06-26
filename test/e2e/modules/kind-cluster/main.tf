locals {
  # NodePorts the kind cluster exposes via extra_port_mapping. Fixed
  # so the harbor module and any caller can construct image refs of
  # the form host:30843. Harbor binds here directly via its own
  # NodePort Service.
  https_node_port_internal = 30843
  http_node_port_internal  = 30880

  # Containerd hosts.toml content for each insecure registry. Written
  # to the host via `docker exec` after kind is up — see docs/E2E-MANUAL-SETUP.md
  # §4 for why this is needed.
  containerd_hosts_files = {
    for host in var.registry_insecure_hostnames :
    host => <<-TOML
      server = "https://${host}"
      [host."https://${host}"]
        capabilities = ["pull", "resolve"]
        skip_verify = true
    TOML
  }

  # NOTE: we cannot use kind's extra_mounts to deliver these — Docker's
  # `-v src:dst:opts` parser treats the `:` in `host:port` as the
  # options delimiter and rejects the mount. Instead, the null_resource
  # below docker-execs into each node after creation and writes the
  # files in place.
}

resource "kind_cluster" "this" {
  name = var.name
  # Cannot wait for nodes Ready — Cilium (installed below) is the CNI
  # and it needs the apiserver responding, which only happens after
  # kind reports the cluster as "up but unready".
  wait_for_ready  = false
  kubeconfig_path = pathexpand("~/.kube/config")
  node_image      = var.node_image

  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"

    networking {
      kube_proxy_mode     = "none"
      disable_default_cni = true
      api_server_address  = "127.0.0.1"
      api_server_port     = var.api_server_port
    }

    feature_gates = {
      KubeletServiceAccountTokenForCredentialProviders = true
      ServiceAccountNodeAudienceRestriction            = true
    }

    # Tell containerd to read per-registry overrides from certs.d.
    # Individual hosts.toml files are mounted in below.
    # Patch BOTH plugin paths so this works regardless of containerd
    # major version:
    #   - v1.x lives at plugins."io.containerd.grpc.v1.cri".registry
    #   - v2.x lives at plugins."io.containerd.cri.v1.images".registry
    # The unused path is silently ignored. kindest/node:v1.35.0 ships
    # containerd 2.x — v1 path alone is no-op there and TLS skip_verify
    # never reaches the image pull.
    containerd_config_patches = [
      <<-TOML
      [plugins."io.containerd.grpc.v1.cri".registry]
        config_path = "/etc/containerd/certs.d"

      [plugins."io.containerd.cri.v1.images".registry]
        config_path = "/etc/containerd/certs.d"
      TOML
    ]

    node {
      role = "control-plane"
      kubeadm_config_patches = [
        <<-EOT
          kind: InitConfiguration
          nodeRegistration:
            kubeletExtraArgs:
              node-labels: "ingress-ready=true"
        EOT
        ,
        <<-EOT
          kind: ClusterConfiguration
          apiServer:
            extraArgs:
              enable-admission-plugins: NodeRestriction,MutatingAdmissionWebhook,ValidatingAdmissionWebhook
        EOT
      ]
      extra_port_mappings {
        host_port      = var.http_port
        container_port = local.http_node_port_internal
        listen_address = "127.0.0.1"
        protocol       = "TCP"
      }
      extra_port_mappings {
        host_port      = var.https_port
        container_port = local.https_node_port_internal
        listen_address = "127.0.0.1"
        protocol       = "TCP"
      }
    }

    dynamic "node" {
      for_each = toset([for i in range(var.worker_count) : tostring(i)])
      content {
        role = "worker"
      }
    }
  }
}

# Resolve all nodes in the cluster to docker container names so the
# null_resource below can iterate over them. kind names containers
# <cluster>-control-plane and <cluster>-worker, <cluster>-worker2, …
locals {
  node_names = concat(
    ["${var.name}-control-plane"],
    [for i in range(var.worker_count) : i == 0 ? "${var.name}-worker" : "${var.name}-worker${i + 1}"],
  )
}

# Write hosts.toml files into each node after the cluster is up.
# Docker can't bind-mount these in via kind extra_mounts because the
# `:port` in the container path breaks `docker run -v`'s parser.
resource "null_resource" "containerd_hosts" {
  for_each = {
    for pair in flatten([
      for host, body in local.containerd_hosts_files : [
        for node in local.node_names : {
          key  = "${node}/${host}"
          node = node
          host = host
          body = body
        }
      ]
    ]) : pair.key => pair
  }

  triggers = {
    node          = each.value.node
    host          = each.value.host
    content_sha   = sha256(each.value.body)
    cluster_token = kind_cluster.this.name # re-create if cluster is replaced
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail
      docker exec '${each.value.node}' mkdir -p '/etc/containerd/certs.d/${each.value.host}'
      docker exec '${each.value.node}' sh -c 'cat > "/etc/containerd/certs.d/${each.value.host}/hosts.toml"' <<'EOF'
${each.value.body}
EOF
      docker exec '${each.value.node}' systemctl restart containerd
    BASH
  }

  depends_on = [kind_cluster.this]
}

# Append /etc/hosts entries on every node. Containerd reads /etc/hosts
# for any hostname not covered by certs.d hosts.toml routing, and the
# auth-realm follow on a 401 from Harbor needs to be able to resolve
# the externalURL host on the kind node itself.
resource "null_resource" "etc_hosts" {
  for_each = {
    for pair in flatten([
      for entry in var.extra_etc_hosts : [
        for node in local.node_names : {
          key   = "${node}/${entry}"
          node  = node
          entry = entry
        }
      ]
    ]) : pair.key => pair
  }

  triggers = {
    node          = each.value.node
    entry         = each.value.entry
    cluster_token = kind_cluster.this.name
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail
      # Idempotent: only append if the entry isn't already present.
      docker exec '${each.value.node}' sh -c '
        grep -qxF "${each.value.entry}" /etc/hosts || echo "${each.value.entry}" >> /etc/hosts
      '
    BASH
  }

  depends_on = [kind_cluster.this]
}

# Load locally-built docker images into the kind cluster so the test
# install can use them without a registry round-trip. Each
# `kind load docker-image` copies the image tarball from the local
# docker daemon into containerd on every node.
resource "null_resource" "kind_load" {
  for_each = toset(var.images_to_load)

  triggers = {
    image_ref     = each.value
    cluster_token = kind_cluster.this.name
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "kind load docker-image '${each.value}' --name '${kind_cluster.this.name}'"
  }

  # Must wait for containerd_hosts: that resource restarts containerd
  # on each node, and `kind load` shells in to ctr to read the
  # snapshotter plugin list. A mid-restart query returns "failed to
  # detect containerd snapshotter" and aborts the load. Slower CI
  # runners lose this race; local runs typically finish the restart in
  # time. Cilium has the same dep below for the same reason.
  depends_on = [kind_cluster.this, null_resource.containerd_hosts]
}

# Cilium replaces kube-proxy and provides the CNI. Wait for the
# containerd hosts.toml + restart to land first so cilium pods can pull
# without complaints, and so the containerd restart doesn't kill the
# already-running cilium pods.
resource "helm_release" "cilium" {
  name            = "cilium"
  namespace       = "kube-system"
  repository      = "https://helm.cilium.io"
  chart           = "cilium"
  version         = "1.19.5"
  timeout         = 600
  wait            = true
  wait_for_jobs   = true
  cleanup_on_fail = true
  atomic          = true

  values = [yamlencode({
    kubeProxyReplacement = "true"
    k8sServiceHost       = "${var.name}-control-plane"
    k8sServicePort       = 6443
    nodePort             = { enabled = true }
    externalIPs          = { enabled = true }
    hostPort             = { enabled = true }
    ipam                 = { mode = "kubernetes" }
    image                = { pullPolicy = "IfNotPresent" }
  })]

  depends_on = [kind_cluster.this, null_resource.containerd_hosts]
}

resource "helm_release" "cert_manager" {
  count            = var.enable_cert_manager ? 1 : 0
  name             = "cert-manager"
  namespace        = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = "v1.20.3"
  create_namespace = true
  timeout          = 600
  wait             = true
  wait_for_jobs    = true
  atomic           = true

  values = [yamlencode({
    crds = { enabled = true }
  })]

  depends_on = [helm_release.cilium]
}
