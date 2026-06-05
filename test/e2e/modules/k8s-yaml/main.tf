terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.30"
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
  type = list(object({
    yaml = string
    # Optional per-manifest wait. Mirrors the kubernetes_manifest
    # `wait { condition { type/status } fields {...} }` schema.
    # Omit `wait` entirely for fire-and-forget manifests (Namespace,
    # Secret, ConfigMap, …) that have no meaningful conditions.
    wait = optional(object({
      conditions = optional(list(object({
        type   = string
        status = string
      })), [])
      fields = optional(map(string), {})
    }), null)
  }))
  description = <<-EOT
    List of YAML documents to apply. Each entry's `yaml` is a single
    Kubernetes object's YAML. Set the optional `wait` field to block
    until the object's status reaches a desired condition / field.
  EOT
}

variable "depends_on_resources" {
  type        = list(any)
  default     = []
  description = "Use to wire ordering when downstream modules need to wait for these manifests."
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

locals {
  # Split namespaces from everything else so namespaces apply first.
  # kubernetes_manifest with for_each parallelises all instances, which
  # races: SAs / Secrets in a yet-to-exist namespace fail with
  # "namespaces XYZ not found". Two resource blocks with depends_on
  # serialises namespace creation before its tenants.
  namespaces = { for i, m in var.manifests : i => m if yamldecode(m.yaml).kind == "Namespace" }
  tenants    = { for i, m in var.manifests : i => m if yamldecode(m.yaml).kind != "Namespace" }
}

resource "kubernetes_manifest" "namespaces" {
  for_each = local.namespaces
  manifest = yamldecode(each.value.yaml)

  field_manager {
    name            = "tofu-e2e-k8s-yaml"
    force_conflicts = true
  }

  dynamic "wait" {
    for_each = each.value.wait != null ? [each.value.wait] : []
    iterator = w
    content {
      dynamic "condition" {
        for_each = w.value.conditions
        content {
          type   = condition.value.type
          status = condition.value.status
        }
      }
      fields = length(w.value.fields) > 0 ? w.value.fields : null
    }
  }
}

resource "kubernetes_manifest" "tenants" {
  for_each = local.tenants
  manifest = yamldecode(each.value.yaml)

  # force_conflicts so server-side apply wins over any leftover field
  # ownership from controllers (e.g. the bridge controller writes
  # status.conditions on HarborAccess and we don't want to fight it).
  field_manager {
    name            = "tofu-e2e-k8s-yaml"
    force_conflicts = true
  }

  dynamic "wait" {
    for_each = each.value.wait != null ? [each.value.wait] : []
    iterator = w
    content {
      dynamic "condition" {
        for_each = w.value.conditions
        content {
          type   = condition.value.type
          status = condition.value.status
        }
      }
      fields = length(w.value.fields) > 0 ? w.value.fields : null
    }
  }

  depends_on = [kubernetes_manifest.namespaces]
}
