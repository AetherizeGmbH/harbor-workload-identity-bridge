locals {
  kubeconfig_path = coalesce(var.kubeconfig_path, abspath("${path.cwd}/.gen/${var.name}.kubeconfig"))

  # NodePorts the kind cluster exposes via extra_port_mapping. Fixed
  # so the harbor module and any caller can construct image refs of
  # the form host:30843. Harbor binds here directly via its own
  # NodePort Service.
  https_node_port_internal = 30843
  http_node_port_internal  = 30880

  # Containerd hosts.toml content for each insecure registry. Written
  # to the host via `docker exec` after kind is up (see the NOTE below for
  # why extra_mounts cannot deliver them).
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

# The cluster is created with the kind CLI, not with the tehcyx/kind
# provider: its latest release (0.11.0) embeds kind v0.31, which writes the
# kubeadm v1beta3 config that kubeadm 1.37 no longer accepts, so the harness
# could not follow kindest/node past v1.36. The CLI version is pinned where
# the binary is installed (CI: kind-action `version`, tracked by Renovate)
# and must support var.node_image (kind v0.33+ for Kubernetes 1.37).
locals {
  kind_config = yamlencode({
    kind       = "Cluster"
    apiVersion = "kind.x-k8s.io/v1alpha4"
    networking = {
      kubeProxyMode     = "none"
      disableDefaultCNI = true
      apiServerAddress  = "127.0.0.1"
      apiServerPort     = var.api_server_port
    }
    featureGates = {
      KubeletServiceAccountTokenForCredentialProviders = true
      ServiceAccountNodeAudienceRestriction            = true
    }
    # Tell containerd to read per-registry overrides from certs.d.
    # Individual hosts.toml files are written in below.
    # Patch BOTH plugin paths so this works regardless of containerd
    # major version:
    #   - v1.x lives at plugins."io.containerd.grpc.v1.cri".registry
    #   - v2.x lives at plugins."io.containerd.cri.v1.images".registry
    # The unused path is silently ignored. kindest/node:v1.35.0+ ships
    # containerd 2.x — v1 path alone is no-op there and TLS skip_verify
    # never reaches the image pull.
    containerdConfigPatches = [
      <<-TOML
      [plugins."io.containerd.grpc.v1.cri".registry]
        config_path = "/etc/containerd/certs.d"
      [plugins."io.containerd.cri.v1.images".registry]
        config_path = "/etc/containerd/certs.d"
      TOML
    ]
    nodes = concat(
      [{
        role  = "control-plane"
        image = var.node_image
        kubeadmConfigPatches = [
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
        extraPortMappings = [
          {
            hostPort      = var.http_port
            containerPort = local.http_node_port_internal
            listenAddress = "127.0.0.1"
            protocol      = "TCP"
          },
          {
            hostPort      = var.https_port
            containerPort = local.https_node_port_internal
            listenAddress = "127.0.0.1"
            protocol      = "TCP"
          },
        ]
      }],
      [for i in range(var.worker_count) : { role = "worker", image = var.node_image }],
    )
  })
}

resource "null_resource" "cluster" {
  triggers = {
    name            = var.name
    kind_config     = local.kind_config
    kubeconfig_path = local.kubeconfig_path
  }

  # Never write into the operator's ~/.kube/config: kind would switch the
  # current context to this throwaway cluster (and drop it again on
  # destroy), and anything that later trusts "the current context" could
  # hit an unrelated — possibly production — cluster. --kubeconfig makes
  # kind write ONLY the harness file; every consumer gets explicit
  # connection info via the kubeconfig output.
  #
  # No --wait: nodes stay NotReady until Cilium (installed below) provides
  # the CNI, and Cilium only needs the apiserver, which kind waits for.
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail
      kind version
      if kind get clusters | grep -qxF "$NAME"; then
        echo "a kind cluster named $NAME already exists (a paused or crashed run?); delete it with: kind delete cluster --name $NAME" >&2
        exit 1
      fi
      mkdir -p "$(dirname "$KUBECONFIG_PATH")"
      printf '%s' "$KIND_CONFIG" | kind create cluster --name "$NAME" --kubeconfig "$KUBECONFIG_PATH" --config -
    BASH
    environment = {
      NAME            = self.triggers.name
      KIND_CONFIG     = self.triggers.kind_config
      KUBECONFIG_PATH = self.triggers.kubeconfig_path
    }
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]
    command     = "kind delete cluster --name \"$NAME\" --kubeconfig \"$KUBECONFIG_PATH\""
    environment = {
      NAME            = self.triggers.name
      KUBECONFIG_PATH = self.triggers.kubeconfig_path
    }
  }
}

# Read back the credentials kind wrote. Deferred to apply (depends on the
# cluster), like the provider's computed attributes were.
data "local_sensitive_file" "kubeconfig" {
  filename   = local.kubeconfig_path
  depends_on = [null_resource.cluster]
}

locals {
  kubeconfig_doc = yamldecode(data.local_sensitive_file.kubeconfig.content)
  cluster_conn = {
    host                   = local.kubeconfig_doc.clusters[0].cluster.server
    cluster_ca_certificate = base64decode(local.kubeconfig_doc.clusters[0].cluster["certificate-authority-data"])
    client_certificate     = base64decode(local.kubeconfig_doc.users[0].user["client-certificate-data"])
    client_key             = base64decode(local.kubeconfig_doc.users[0].user["client-key-data"])
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
    cluster_token = null_resource.cluster.id # re-create if cluster is replaced
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

  depends_on = [null_resource.cluster]
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
    cluster_token = null_resource.cluster.id
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

  depends_on = [null_resource.cluster]
}

# Load locally-built docker images into the kind cluster so the test
# install can use them without a registry round-trip. Each
# `kind load docker-image` copies the image tarball from the local
# docker daemon into containerd on every node.
resource "null_resource" "kind_load" {
  for_each = toset(var.images_to_load)

  triggers = {
    image_ref     = each.value
    cluster_token = null_resource.cluster.id
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "kind load docker-image '${each.value}' --name '${var.name}'"
  }

  # Must wait for containerd_hosts: that resource restarts containerd
  # on each node, and `kind load` shells in to ctr to read the
  # snapshotter plugin list. A mid-restart query returns "failed to
  # detect containerd snapshotter" and aborts the load. Slower CI
  # runners lose this race; local runs typically finish the restart in
  # time. Cilium has the same dep below for the same reason.
  depends_on = [null_resource.cluster, null_resource.containerd_hosts]
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
  version         = "1.20.2"
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

  depends_on = [null_resource.cluster, null_resource.containerd_hosts]
}

resource "helm_release" "cert_manager" {
  count            = var.enable_cert_manager ? 1 : 0
  name             = "cert-manager"
  namespace        = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = "v1.21.2"
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
