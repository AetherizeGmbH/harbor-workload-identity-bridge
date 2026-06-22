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
  default     = "harbor.e2e"
  description = <<-EOT
    Hostname used in Harbor's externalURL and propagated to the
    docker-registry www-authenticate realm.

    Cannot be 127.0.0.1 or any RFC1918-private IP: go-containerregistry
    (crane / containerd's image puller) hard-rejects loopback /
    private realms with `invalid realm in www-authenticate: realm
    host "X" is a private or link-local address`. Use a synthetic
    TLD-less name and resolve it both inside-cluster (CoreDNS
    rewrite or pod hostAlias) and on kind nodes (containerd
    hosts.toml). The kind_node_ip output below gives callers a
    routable IP for the pod hostAlias.
  EOT
}

variable "version_harbor" {
  type        = string
  default     = null
  description = "Harbor Helm chart version to install. null → the renovate-pinned default in locals below (the normal path; the harbor-compat matrix overrides it per ADR-0020)."
}

locals {
  # renovate: datasource=helm depName=harbor registryUrl=https://helm.goharbor.io
  default_version_harbor = "1.19.1"
  # Single source of truth for the pin stays the line above so Renovate keeps
  # bumping it; callers (incl. the compat matrix) override via var.version_harbor.
  version_harbor = coalesce(var.version_harbor, local.default_version_harbor)
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
  version          = local.version_harbor
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
