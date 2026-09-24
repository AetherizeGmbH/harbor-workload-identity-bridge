# TEST-ONLY scaffolding (ADR-0022): make every GKE node's containerd
# trust Harbor's self-signed certificate via a per-registry hosts.toml.
# hosts.toml is read per pull (no containerd restart); only when the
# node's containerd config has NO registry config_path at all does the
# DaemonSet append one and restart containerd once (running containers
# survive a containerd restart — the shims own them).
#
# This is the GKE analogue of ../e2e/modules/containerd-registry-trust,
# which docker-execs into kind nodes. On GKE there is no docker exec,
# so a privileged DaemonSet with a host-root mount does the same work,
# then sleeps to keep the DS Ready (wait_for_rollout is the barrier
# the tftest sequencing relies on).
#
# NOTHING here ships to users: production registries bring real certs;
# registry TLS trust is orthogonal to what the bridge does.
terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
  }
}

variable "kubeconfig" {
  type = object({
    host                   = string
    cluster_ca_certificate = string
    client_certificate     = optional(string)
    client_key             = optional(string)
    token                  = optional(string)
  })
  sensitive = true
}

variable "registry_hostname" {
  type        = string
  description = "Registry hostname as it appears in image refs (443 implicit, so also the certs.d directory name), e.g. harbor.<ip>.sslip.io."
}

variable "connect_addr" {
  type        = string
  description = "host:port openssl s_client fetches the serving cert from (e.g. \"<lb-ip>:443\")."
}

variable "image" {
  type        = string
  description = "Image with sh + openssl + nsenter userland (the harness reuses the AR-pushed e2e-seed image — pulling it also smoke-tests GKE's own AR credential provider before the bridge is installed)."
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

locals {
  script = <<-SH
    set -eu
    CONFIG=/host/etc/containerd/config.toml
    HOST='${var.registry_hostname}'

    # Respect an existing config_path (GKE may manage one); only wire
    # our own when the config has no registry section at all. A registry
    # section WITHOUT config_path is ambiguous — appending a duplicate
    # section would corrupt the TOML and take containerd down, so fail
    # loudly instead (runtime finding for ADR-0022).
    DIR=$(sed -n 's/^[[:space:]]*config_path[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$CONFIG" | head -1)
    RESTART=""
    if [ -z "$DIR" ]; then
      if grep -q 'registry\]' "$CONFIG"; then
        echo "ERROR: containerd config has a registry section but no config_path — refusing to append a duplicate section" >&2
        exit 1
      fi
      DIR=/etc/containerd/certs.d
      printf '\n[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "%s"\n' "$DIR" >> "$CONFIG"
      RESTART=1
    fi
    echo "registry config_path: $DIR"

    mkdir -p "/host$DIR/$HOST"
    # Grab the serving cert off the wire — same house style as the kind
    # harness and the seed Job (chart secret naming is fragile).
    openssl s_client -connect '${var.connect_addr}' -servername "$HOST" </dev/null 2>/dev/null \
      | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
      > "/host$DIR/$HOST/ca.crt"
    test -s "/host$DIR/$HOST/ca.crt" || {
      echo "ERROR: no certificate from ${var.connect_addr}" >&2
      exit 1
    }
    cat > "/host$DIR/$HOST/hosts.toml" <<EOF
    server = "https://$HOST"

    [host."https://$HOST"]
      capabilities = ["pull", "resolve"]
      ca = "$DIR/$HOST/ca.crt"
    EOF

    if [ -n "$RESTART" ]; then
      echo "restarting containerd to activate config_path"
      nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd
    fi
    echo "containerd trust installed for $HOST"
    exec sleep infinity
  SH
}

resource "kubernetes_namespace_v1" "this" {
  metadata {
    name = "e2e-containerd-trust"
  }
}

resource "kubernetes_daemon_set_v1" "trust" {
  metadata {
    name      = "containerd-trust"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  # wait_for_rollout (default true) blocks until every node ran the
  # script and the pod is Ready — the sequencing barrier for the seed
  # and pull stages.
  spec {
    selector {
      match_labels = { app = "containerd-trust" }
    }
    template {
      metadata {
        labels = { app = "containerd-trust" }
      }
      spec {
        host_pid = true
        toleration {
          operator = "Exists"
        }
        container {
          name    = "trust"
          image   = var.image
          command = ["sh", "-c", local.script]
          security_context {
            privileged = true
          }
          volume_mount {
            name       = "host-root"
            mount_path = "/host"
          }
          resources {
            requests = {
              cpu    = "10m"
              memory = "16Mi"
            }
            limits = {
              cpu    = "100m"
              memory = "64Mi"
            }
          }
        }
        volume {
          name = "host-root"
          host_path {
            path = "/"
          }
        }
      }
    }
  }

  timeouts {
    create = "10m"
    update = "10m"
  }
}
