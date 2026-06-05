terraform {
  required_version = ">= 1.6"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.32"
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
  default = {
    repository = "harbor-bridge"
    tag        = "e2e"
  }
}

variable "plugin_image" {
  type = object({
    repository = string
    tag        = string
  })
  default = {
    repository = "harbor-bridge-plugin"
    tag        = "e2e"
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
      name: harbor-bridge-ca
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
      replicas = 1
      logLevel = "debug"
      image    = var.bridge_image
    }
    tls = {
      enabled = true
      issuerRef = {
        name = "harbor-bridge-ca"
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
