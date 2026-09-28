# Points containerd on every kind node at plain-HTTP registries: one
# hosts.toml per host:port under /etc/containerd/certs.d (the config_path
# the kind-cluster module sets), with an http:// server, the way kind's
# own local-registry setup does. The Nexus harness uses it for the HTTP
# docker connectors of its hosted repositories; Harbor keeps TLS
# (containerd-registry-trust).
#
# containerd restarts once per node after the files are written, as in
# the other modules that write hosts.toml.
terraform {
  required_version = ">= 1.6"
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.0"
    }
  }
}

variable "cluster_name" {
  type        = string
  description = "Kind cluster name. Only used to bust the trigger when the cluster is recreated."
}

variable "node_names" {
  type        = list(string)
  description = "Docker container names of all kind nodes."
}

variable "registry_host_ports" {
  type        = list(string)
  description = "Registry host:port values as image references name them (e.g. \"nexus.e2e:30851\")."

  validation {
    condition     = length(var.registry_host_ports) > 0 && alltrue([for h in var.registry_host_ports : can(regex("^[a-z0-9][a-z0-9.-]*:[0-9]+$", h))])
    error_message = "registry_host_ports needs at least one entry, each host:port with a lower-case host name."
  }
}

locals {
  # The hosts.toml of each registry, one line per list element: printf
  # writes them, so no heredoc nests inside the provisioner's heredoc.
  hosts_toml = {
    for h in var.registry_host_ports : h => [
      "server = \"http://${h}\"",
      "",
      "[host.\"http://${h}\"]",
      "  capabilities = [\"pull\", \"resolve\"]",
    ]
  }

  # One shell line per registry: create the directory, write the file.
  write_hosts = join("\n", [
    for h, lines in local.hosts_toml : join(" ", [
      "docker exec \"$NODE\" mkdir -p '/etc/containerd/certs.d/${h}' &&",
      "printf '%s\\n' ${join(" ", [for l in lines : "'${l}'"])}",
      "| docker exec -i \"$NODE\" sh -c \"cat > '/etc/containerd/certs.d/${h}/hosts.toml'\"",
    ])
  ])
}

resource "null_resource" "containerd_http" {
  for_each = toset(var.node_names)

  triggers = {
    node          = each.value
    hosts_toml    = sha256(jsonencode(local.hosts_toml))
    cluster_token = var.cluster_name
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail
      NODE='${each.value}'
      ${local.write_hosts}
      docker exec "$NODE" systemctl restart containerd
      echo "OK: containerd on $NODE reaches ${join(", ", var.registry_host_ports)} over plain HTTP"
    BASH
  }
}
