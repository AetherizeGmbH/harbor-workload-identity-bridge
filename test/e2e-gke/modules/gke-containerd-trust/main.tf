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
# then sleeps. The script's last step writes a marker that the readiness
# probe waits for, so a pod turns Ready only after the whole script,
# containerd restart included, has succeeded: wait_for_rollout is the
# barrier the tftest sequencing relies on, and a failing script fails
# this stage instead of surfacing later as a pull error.
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
  # Written as the script's last step; the readiness probe waits for it.
  done_marker = "/tmp/containerd-trust-done"

  script = <<-SH
    set -eu
    CONFIG=/host/etc/containerd/config.toml
    HOST='${var.registry_hostname}'
    # Exists while a config_path we appended is not yet active, i.e.
    # containerd has not been restarted since. It sits next to the config
    # (same lifetime), so if this container dies between the append and
    # the restart, the rerun (which then finds our config_path) still
    # restarts containerd.
    PENDING="$CONFIG.e2e-trust-restart-pending"
    test -f "$CONFIG" || {
      echo "ERROR: $CONFIG not found" >&2
      exit 1
    }

    # Runtime finding for ADR-0022: the config schema GKE ships.
    echo "containerd config: $(grep -m1 -E '^[[:space:]]*version[[:space:]]*=' "$CONFIG" || echo 'no version line')"

    # Respect an existing config_path (GKE may manage one); only wire
    # our own when the config has no registry settings at all.
    DIR=$(sed -n 's/^[[:space:]]*config_path[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$CONFIG" | head -1)
    SECTION=""
    if [ -z "$DIR" ]; then
      # Any registry table without config_path (registry, registry.mirrors,
      # registry.configs, ...) is ambiguous: appending ours would duplicate
      # a table (invalid TOML) or set config_path next to mirrors, which
      # containerd rejects. Either takes containerd down, so fail loudly.
      if grep -Eq '[.]registry[].]' "$CONFIG"; then
        echo "ERROR: containerd config has registry settings but no config_path; refusing to edit it" >&2
        exit 1
      fi
      # The CRI registry section depends on the config schema: version 2
      # (containerd 1.x; 2.x migrates it) reads grpc.v1.cri, version 3
      # (containerd 2.x) reads cri.v1.images and ignores the old path.
      VERSION=$(sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$CONFIG" | head -1)
      case "$VERSION" in
        2) SECTION='plugins."io.containerd.grpc.v1.cri".registry' ;;
        3) SECTION='plugins."io.containerd.cri.v1.images".registry' ;;
        *)
          echo "ERROR: containerd config version '$VERSION' is not 2 or 3; refusing to guess the registry section" >&2
          exit 1
          ;;
      esac
      DIR=/etc/containerd/certs.d
    fi
    echo "registry config_path: $DIR"

    # Trust material first. It is only read once config_path points at
    # it, and a failure here (e.g. the LB not forwarding yet) leaves
    # config.toml untouched for the retry.
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

    # Wire config_path last, then activate it.
    if [ -n "$SECTION" ]; then
      touch "$PENDING"
      printf '\n[%s]\n  config_path = "%s"\n' "$SECTION" "$DIR" >> "$CONFIG"
      echo "appended [$SECTION] config_path = \"$DIR\""
    fi
    if [ -e "$PENDING" ]; then
      echo "restarting containerd to activate config_path"
      nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd
      rm -f "$PENDING"
    fi
    echo "containerd trust installed for $HOST"
    touch '${local.done_marker}'
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
  # wait_for_rollout (default true) blocks until every pod is Ready, and
  # the readiness probe turns a pod Ready only after its script wrote the
  # done marker: every node is trusted (and containerd restarted where
  # needed) before the seed and pull stages start. A failing script never
  # turns Ready, so this stage times out instead of passing.
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
          readiness_probe {
            exec {
              command = ["test", "-f", local.done_marker]
            }
            period_seconds = 2
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
