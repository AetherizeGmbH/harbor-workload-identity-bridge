terraform {
  required_version = ">= 1.6"
  required_providers {
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

variable "manifests" {
  type        = list(string)
  description = <<-EOT
    List of YAML documents to apply. Pass each `kind:` separately —
    each entry is one server-side-applied object.
  EOT
}

variable "depends_on_resources" {
  type        = list(any)
  default     = []
  description = "Use to wire ordering when downstream modules need to wait for these manifests."
}

provider "kubectl" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  load_config_file       = false
}

resource "kubectl_manifest" "this" {
  for_each  = { for i, m in var.manifests : i => m }
  yaml_body = each.value

  # Tolerate the apiserver hiccupping during kubelet restarts.
  server_side_apply = true
  field_manager     = "tofu-e2e-k8s-yaml"
}
