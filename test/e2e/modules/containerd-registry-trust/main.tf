terraform {
  required_version = ">= 1.6"
  # null provider is built-in; no external providers needed.
}

variable "cluster_name" {
  type        = string
  description = "Kind cluster name. Only used to bust the trigger when the cluster is recreated."
}

variable "node_names" {
  type        = list(string)
  description = "Docker container names of all kind nodes."
}

variable "registry_host_port" {
  type        = string
  description = "Registry host:port as it appears in image refs and the containerd certs.d directory name (e.g. \"harbor.e2e:30843\")."
}

variable "extract_from_node_port" {
  type        = string
  description = "host:port that `openssl s_client` connects to from inside a kind node (typically \"127.0.0.1:<node_port>\"). The cert returned is what containerd will see at pull time."
}

locals {
  # Hostname portion of registry_host_port, used as the SNI servername.
  servername = split(":", var.registry_host_port)[0]

  # Full hosts.toml content. Uses `ca` (not skip_verify) because v2.x
  # containerd has been observed to silently ignore skip_verify even
  # when hosts.toml is otherwise loaded.
  hosts_toml = <<-TOML
    server = "https://${var.registry_host_port}"

    [host."https://${var.registry_host_port}"]
      capabilities = ["pull", "resolve"]
      ca = "/etc/containerd/certs.d/${var.registry_host_port}/ca.crt"
  TOML
}

resource "null_resource" "containerd_trust" {
  for_each = toset(var.node_names)

  triggers = {
    node          = each.value
    registry      = var.registry_host_port
    hosts_toml    = sha256(local.hosts_toml)
    cluster_token = var.cluster_name
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail

      NODE='${each.value}'
      DIR='/etc/containerd/certs.d/${var.registry_host_port}'

      # Extract the cert from inside the node — getting it via openssl
      # at the same host:port that containerd will hit guarantees we
      # trust *exactly* what the server presents (no aliasing surprises).
      CERT=$(docker exec "$NODE" sh -c \
        "openssl s_client -connect ${var.extract_from_node_port} -servername ${local.servername} </dev/null 2>/dev/null" \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p')
      if [ -z "$CERT" ]; then
        echo "ERROR: openssl s_client returned no cert from ${var.extract_from_node_port} on $NODE" >&2
        exit 1
      fi

      docker exec "$NODE" mkdir -p "$DIR"
      printf '%s\n' "$CERT" | docker exec -i "$NODE" sh -c "cat > '$DIR/ca.crt'"
      docker exec -i "$NODE" sh -c "cat > '$DIR/hosts.toml'" <<'HOSTS'
${local.hosts_toml}
HOSTS

      docker exec "$NODE" systemctl restart containerd
      echo "OK: containerd trust installed on $NODE for ${var.registry_host_port}"
    BASH
  }
}
