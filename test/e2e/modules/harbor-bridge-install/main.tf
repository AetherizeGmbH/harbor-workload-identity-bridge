terraform {
  required_version = ">= 1.6"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
    kubectl = {
      source  = "alekc/kubectl"
      version = "~> 2.1"
    }
  }
}

variable "kubeconfig" {
  type = object({
    host                   = string
    client_certificate     = string
    client_key             = string
    cluster_ca_certificate = string
  })
  sensitive = true
}

variable "namespace" {
  type    = string
  default = "harbor-bridge-system"
}

variable "cluster_name" {
  type        = string
  description = "BRIDGE_CLUSTER_NAME — used as robot-account prefix in Harbor."
}

variable "harbor_url" {
  type        = string
  description = "In-cluster Harbor REST API URL (e.g. http://harbor-core.harbor.svc.cluster.local). The bridge talks to this directly over service DNS."
}

variable "harbor_admin_password" {
  type      = string
  sensitive = true
}

variable "audience" {
  type        = string
  description = "Audience the plugin's tokenAttributes uses. Must match HarborAccess.trustPolicy.audience."
}

variable "match_images" {
  type        = list(string)
  description = "kubelet matchImages glob patterns (full registry host:port form)."
}

variable "chart_path" {
  type        = string
  default     = "../../../../charts/harbor-bridge"
  description = <<-EOT
    Relative to this module dir (test/e2e/modules/harbor-bridge-install).
    Four parents up reaches the repo root, then charts/harbor-bridge.
  EOT
}

variable "bridge_image" {
  type = object({
    repository = string
    tag        = string
  })
  description = "Bridge container image. Required. Caller is expected to derive this from the test's docker-build run output so the install exercises the working-tree Dockerfiles instead of a stale tag."
}

variable "plugin_image" {
  type = object({
    repository = string
    tag        = string
  })
  description = "Plugin container image. Required. See bridge_image."
}

variable "issuer_name" {
  type        = string
  default     = "harbor-bridge-ca"
  description = "Name of the self-signed cert-manager ClusterIssuer created for the bridge's serving cert. Referenced by both the ClusterIssuer manifest and the chart's tls.issuerRef."
}

# Resource requests/limits for the bridge Deployment container, wired into
# the chart's bridge.resources. Defaults mirror the chart's own defaults
# (Burstable QoS: limits.memory ≫ requests.memory). Memory must be in Mi or
# Gi format (validated below); CPU is free-form.
variable "bridge_resources" {
  type = object({
    requests = optional(object({
      cpu    = optional(string, "50m")
      memory = optional(string, "64Mi")
    }), {})
    limits = optional(object({
      cpu    = optional(string, "500m")
      memory = optional(string, "256Mi")
    }), {})
  })
  default     = {}
  nullable    = false
  description = "Resource requests/limits for the bridge Deployment container, fed straight into the chart's bridge.resources. Memory must be in Mi or Gi format."

  validation {
    condition = alltrue([
      for m in [var.bridge_resources.requests.memory, var.bridge_resources.limits.memory] :
      can(regex("^[0-9]+(Mi|Gi)$", m))
    ])
    error_message = "requests.memory and limits.memory must be in Mi or Gi format (e.g. \"64Mi\" or \"1Gi\")."
  }
}

provider "helm" {
  kubernetes = {
    host                   = var.kubeconfig.host
    client_certificate     = var.kubeconfig.client_certificate
    client_key             = var.kubeconfig.client_key
    cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  }
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

provider "kubectl" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  load_config_file       = false
}

resource "kubernetes_namespace_v1" "this" {
  metadata { name = var.namespace }
}

resource "kubernetes_secret_v1" "admin" {
  metadata {
    name      = "harbor-admin"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = {
    username = "admin"
    password = var.harbor_admin_password
  }
  type = "Opaque"
}

resource "kubectl_manifest" "cluster_issuer" {
  yaml_body         = <<-YAML
    apiVersion: cert-manager.io/v1
    kind: ClusterIssuer
    metadata:
      name: ${var.issuer_name}
    spec:
      selfSigned: {}
  YAML
  server_side_apply = true
  field_manager     = "tofu-e2e-harbor-bridge-install"
}

# CRDs are installed automatically by helm from the chart's crds/
# directory on first install. No separate kubectl_manifest needed.

resource "helm_release" "bridge" {
  name      = "harbor-bridge"
  namespace = kubernetes_namespace_v1.this.metadata[0].name
  chart     = abspath("${path.module}/${var.chart_path}")
  timeout   = 600
  wait      = true
  atomic    = false # the plugin DaemonSet restarts kubelet — be patient

  values = [yamlencode({
    clusterName = var.cluster_name
    harbor = {
      url = var.harbor_url
      adminCredsSecret = {
        name = kubernetes_secret_v1.admin.metadata[0].name
      }
    }
    plugin = {
      matchImages  = var.match_images
      audience     = var.audience
      patchKubelet = true
      image        = var.plugin_image
    }
    bridge = {
      replicas  = 1
      logLevel  = "debug"
      image     = var.bridge_image
      resources = var.bridge_resources
    }
    tls = {
      enabled = true
      issuerRef = {
        name = var.issuer_name
        kind = "ClusterIssuer"
      }
    }
  })]

  depends_on = [
    kubectl_manifest.cluster_issuer,
  ]
}

output "namespace" {
  value = kubernetes_namespace_v1.this.metadata[0].name
}

output "bridge_release" {
  value = helm_release.bridge.id
}
