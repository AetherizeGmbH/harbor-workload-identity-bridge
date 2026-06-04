terraform {
  required_version = ">= 1.6"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
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
  default = "traefik"
}

variable "https_node_port" {
  type    = number
  default = 30843
}

variable "http_node_port" {
  type    = number
  default = 30880
}

variable "version_traefik_crds" {
  type    = string
  default = "1.18.0"
}

variable "version_traefik" {
  type    = string
  default = "33.2.1"
}

provider "helm" {
  kubernetes = {
    host                   = var.kubeconfig.host
    client_certificate     = var.kubeconfig.client_certificate
    client_key             = var.kubeconfig.client_key
    cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  }
}

# Traefik's IngressRoute / Middleware / etc. CRDs must exist before the
# main chart installs (otherwise the chart's CRD validation hooks
# fail). The traefik-crds chart is a tiny purpose-built helper.
resource "helm_release" "traefik_crds" {
  name             = "traefik-crds"
  namespace        = var.namespace
  repository       = "https://traefik.github.io/charts"
  chart            = "traefik-crds"
  version          = var.version_traefik_crds
  create_namespace = true
  timeout          = 300
  wait             = true
  atomic           = true
}

resource "helm_release" "traefik" {
  name       = "traefik"
  namespace  = var.namespace
  repository = "https://traefik.github.io/charts"
  chart      = "traefik"
  version    = var.version_traefik
  timeout    = 600
  wait       = true
  atomic     = false # tolerate slow pod startup; helm uninstall on rollback drops the CRDs which we don't want

  values = [yamlencode({
    deployment = { replicas = 1 }
    service    = { type = "NodePort" }
    ports = {
      web = {
        port     = 8000
        nodePort = var.http_node_port
        expose   = { default = true }
      }
      websecure = {
        port     = 8443
        nodePort = var.https_node_port
        expose   = { default = true }
        tls      = { enabled = true }
      }
    }
    ingressClass = {
      enabled        = true
      isDefaultClass = true
    }
    providers = {
      kubernetesIngress = {
        publishedService          = { enabled = true }
        allowExternalNameServices = true
      }
    }
    metrics = { prometheus = { service = { enabled = false } } }
    # Don't install CRDs from the main chart — traefik_crds handles it.
    installCRDs = false
  })]

  depends_on = [helm_release.traefik_crds]
}
