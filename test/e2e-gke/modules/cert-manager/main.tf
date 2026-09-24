# cert-manager for the GKE harness. The kind harness installs it
# inside the kind-cluster module (test/e2e/modules/kind-cluster); the
# version pin below mirrors that one — bump them together.
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
    cluster_ca_certificate = string
    client_certificate     = optional(string)
    client_key             = optional(string)
    token                  = optional(string)
  })
  sensitive = true
}

provider "helm" {
  kubernetes = {
    host                   = var.kubeconfig.host
    client_certificate     = var.kubeconfig.client_certificate
    client_key             = var.kubeconfig.client_key
    token                  = var.kubeconfig.token
    cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  }
}

resource "helm_release" "cert_manager" {
  name             = "cert-manager"
  namespace        = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = "v1.21.2"
  create_namespace = true
  timeout          = 600
  wait             = true
  wait_for_jobs    = true
  atomic           = true

  values = [yamlencode({
    crds = { enabled = true }
  })]
}

output "release" {
  value = helm_release.cert_manager.id
}
