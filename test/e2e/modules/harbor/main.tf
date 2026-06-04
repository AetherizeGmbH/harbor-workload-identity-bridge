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
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
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
  default = "harbor"
}

variable "https_node_port" {
  type        = number
  default     = 30843
  description = "NodePort that Harbor's HTTPS listener binds. Must match kind extra_port_mapping."
}

variable "http_node_port" {
  type    = number
  default = 30880
}

variable "external_hostname" {
  type        = string
  default     = "127.0.0.1"
  description = <<-EOT
    Host that Harbor's auto-generated TLS cert is issued for, and
    that the external URL uses. 127.0.0.1 works for both laptop
    (via kind extra_port_mapping) and on-node containerd (NodePort
    routes via Cilium from any node's host netns).
  EOT
}

variable "version_harbor" {
  type    = string
  default = "1.16.0"
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

resource "random_password" "admin" {
  length  = 24
  special = false
}

resource "helm_release" "harbor" {
  name             = "harbor"
  namespace        = var.namespace
  repository       = "https://helm.goharbor.io"
  chart            = "harbor"
  version          = var.version_harbor
  create_namespace = true
  timeout          = 900
  wait             = true
  wait_for_jobs    = false
  atomic           = false

  values = [yamlencode({
    expose = {
      type = "nodePort"
      tls = {
        enabled    = true
        certSource = "auto"
        auto = {
          commonName = var.external_hostname
        }
      }
      nodePort = {
        name = "harbor"
        ports = {
          http  = { port = 80, nodePort = var.http_node_port }
          https = { port = 443, nodePort = var.https_node_port }
        }
      }
    }
    externalURL         = "https://${var.external_hostname}:${var.https_node_port}"
    harborAdminPassword = random_password.admin.result
    persistence = {
      enabled = true
      persistentVolumeClaim = {
        registry   = { size = "5Gi", storageClass = "" }
        jobservice = { jobLog = { size = "1Gi", storageClass = "" } }
        database   = { size = "1Gi", storageClass = "" }
        redis      = { size = "1Gi", storageClass = "" }
        trivy      = { size = "1Gi", storageClass = "" }
      }
    }
    portal      = { replicas = 1 }
    core        = { replicas = 1 }
    jobservice  = { replicas = 1 }
    registry    = { replicas = 1 }
    trivy       = { enabled = false }
    notary      = { enabled = false }
    chartmuseum = { enabled = false }
  })]
}
