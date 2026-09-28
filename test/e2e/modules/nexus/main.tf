# Sonatype Nexus Repository in the kind cluster (ADR-0033 decision i).
#
#   - One Deployment (image nexus-e2e:3.76.1, built from test/e2e/nexus and
#     kind-loaded) on a PersistentVolumeClaim, so the nexus_outage stage
#     can scale it to zero and back without losing the users, roles and
#     repositories the scenario built.
#   - The admin password comes from random_password through
#     NEXUS_SECURITY_INITIAL_PASSWORD. No EULA exists in 3.76.1 and the
#     harness never accepts one.
#   - The startup probe waits for /service/rest/v1/status/writable, the
#     endpoint Nexus answers 200 on once it accepts writes.
#   - Service "nexus" (ClusterIP) carries the REST API on 8081 for the
#     bridge and the seed and check Jobs. Service "nexus-docker" (NodePort)
#     carries one HTTP docker connector per hosted repository: Nexus 3.76
#     routes docker requests to a repository only by connector port (path
#     routing arrived in 3.83). The seed creates the repositories with
#     these ports; until then the connectors do not listen.
#
# The connectors speak plain HTTP. containerd reaches them through
# containerd-registry-http (hosts.toml with an http:// server), the way
# kind's own local-registry setup does, so no TLS front is needed.
terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
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

variable "namespace" {
  type    = string
  default = "nexus"
}

variable "image" {
  type        = string
  default     = "nexus-e2e:3.76.1"
  description = "Nexus image, kind-loaded (docker-build of test/e2e/nexus/Dockerfile). It exists only on the kind nodes."
}

variable "external_hostname" {
  type        = string
  default     = "nexus.e2e"
  description = "Synthetic host name of the docker connectors in image references. The kind nodes map it to 127.0.0.1 (/etc/hosts), pods to a node IP (CoreDNS)."
}

variable "repositories" {
  type = map(object({
    connector_port = number
    node_port      = number
  }))
  description = "Hosted docker repositories by name: the HTTP connector port inside the pod and the NodePort that exposes it."

  validation {
    condition = alltrue([
      for name, r in var.repositories :
      can(regex("^[a-z0-9][a-z0-9-]*$", name)) && r.connector_port > 1024 && r.connector_port < 65536 && r.connector_port != 8081 && r.node_port >= 30000 && r.node_port <= 32767
    ])
    error_message = "Repository names must be lower-case letters, digits and '-'; connector ports 1025-65535 except 8081; node ports 30000-32767."
  }
  validation {
    condition     = length(distinct([for r in var.repositories : r.connector_port])) == length(var.repositories) && length(distinct([for r in var.repositories : r.node_port])) == length(var.repositories)
    error_message = "Every repository needs its own connector port and node port."
  }
}

variable "heap" {
  type        = string
  default     = "1g"
  description = "JVM heap (-Xms and -Xmx) and direct memory limit. The image's default (2703m each) does not fit next to Harbor on a laptop."
}

locals {
  labels = { "app.kubernetes.io/name" = "nexus", "app.kubernetes.io/part-of" = "bridge-e2e" }
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

resource "random_password" "admin" {
  length  = 24
  special = false
}

resource "kubernetes_namespace_v1" "this" {
  metadata { name = var.namespace }
}

resource "kubernetes_secret_v1" "initial_password" {
  metadata {
    name      = "nexus-initial-password"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = {
    password = random_password.admin.result
  }
  type = "Opaque"
}

resource "kubernetes_persistent_volume_claim_v1" "data" {
  metadata {
    name      = "nexus-data"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    access_modes = ["ReadWriteOnce"]
    resources {
      requests = { storage = "5Gi" }
    }
  }
  # kind's default StorageClass binds on first consumer: waiting here
  # would wait for the Deployment below, which waits for this claim.
  wait_until_bound = false
}

resource "kubernetes_deployment_v1" "nexus" {
  metadata {
    name      = "nexus"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
    labels    = local.labels
  }
  spec {
    replicas = 1
    # One pod at a time on the ReadWriteOnce claim.
    strategy {
      type = "Recreate"
    }
    selector {
      match_labels = local.labels
    }
    template {
      metadata {
        labels = local.labels
      }
      spec {
        # The image runs as nexus (uid/gid 200); fs_group makes the claim
        # writable for it.
        security_context {
          run_as_user     = 200
          run_as_group    = 200
          run_as_non_root = true
          fs_group        = 200
        }
        container {
          name              = "nexus"
          image             = var.image
          image_pull_policy = "IfNotPresent"
          env {
            name  = "INSTALL4J_ADD_VM_PARAMS"
            value = "-Xms${var.heap} -Xmx${var.heap} -XX:MaxDirectMemorySize=${var.heap} -Djava.util.prefs.userRoot=/nexus-data/javaprefs"
          }
          # Read by StaticSecurityConfigurationSource (nexus-public
          # release-3.76.1-01): with it set, Nexus writes no random
          # admin.password file and creates admin as an active user.
          env {
            name = "NEXUS_SECURITY_INITIAL_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.initial_password.metadata[0].name
                key  = "password"
              }
            }
          }
          port {
            name           = "http"
            container_port = 8081
          }
          dynamic "port" {
            for_each = var.repositories
            content {
              name           = "docker-${port.value.connector_port}"
              container_port = port.value.connector_port
            }
          }
          # Nexus needs one to a few minutes to become writable, longer when
          # its amd64 binaries run emulated on an arm64 host. 90 x 10 s.
          startup_probe {
            http_get {
              path = "/service/rest/v1/status/writable"
              port = "http"
            }
            period_seconds    = 10
            timeout_seconds   = 5
            failure_threshold = 90
          }
          readiness_probe {
            http_get {
              path = "/service/rest/v1/status/writable"
              port = "http"
            }
            period_seconds    = 10
            timeout_seconds   = 5
            failure_threshold = 3
          }
          resources {
            requests = {
              cpu    = "500m"
              memory = "1536Mi"
            }
            limits = {
              memory = "3Gi"
            }
          }
          volume_mount {
            name       = "data"
            mount_path = "/nexus-data"
          }
        }
        volume {
          name = "data"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim_v1.data.metadata[0].name
          }
        }
      }
    }
  }
  wait_for_rollout = true
  timeouts {
    create = "20m"
    update = "20m"
  }
}

resource "kubernetes_service_v1" "api" {
  metadata {
    name      = "nexus"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
    labels    = local.labels
  }
  spec {
    type     = "ClusterIP"
    selector = local.labels
    port {
      name        = "http"
      port        = 8081
      target_port = "http"
    }
  }
}

resource "kubernetes_service_v1" "docker" {
  metadata {
    name      = "nexus-docker"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
    labels    = local.labels
  }
  spec {
    type     = "NodePort"
    selector = local.labels
    dynamic "port" {
      for_each = var.repositories
      content {
        # IANA service names allow 15 characters; repository names may
        # be longer.
        name        = "docker-${port.value.connector_port}"
        port        = port.value.connector_port
        target_port = port.value.connector_port
        node_port   = port.value.node_port
      }
    }
  }
}

output "namespace" {
  value = kubernetes_namespace_v1.this.metadata[0].name
}

output "deployment_name" {
  value = kubernetes_deployment_v1.nexus.metadata[0].name
}

output "pod_selector" {
  value       = join(",", [for k, v in local.labels : "${k}=${v}"])
  description = "Label selector of the Nexus pod, for kubectl."
}

output "admin_password" {
  value     = random_password.admin.result
  sensitive = true
}

output "internal_url" {
  value       = "http://${kubernetes_service_v1.api.metadata[0].name}.${kubernetes_namespace_v1.this.metadata[0].name}.svc.cluster.local:8081"
  description = "Base URL of Nexus inside the cluster, without /service/rest (the bridge's nexus.url)."
}

output "registry_hosts" {
  value       = { for name, r in var.repositories : name => "${var.external_hostname}:${r.node_port}" }
  description = "Repository name → host:port of its docker connector as image references name it."
}

output "repositories" {
  value = {
    for name, r in var.repositories : name => {
      connector_port = r.connector_port
      registry_host  = "${var.external_hostname}:${r.node_port}"
    }
  }
  description = "Repository name → connector port and registry host, for the seed."
}
