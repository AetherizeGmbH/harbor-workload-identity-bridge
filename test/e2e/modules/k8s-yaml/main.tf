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
    # Client-cert auth (kind) and token auth (GKE) are alternatives;
    # exactly one pair/field is expected to be set.
    client_certificate = optional(string)
    client_key         = optional(string)
    token              = optional(string)
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
    Must not carry sensitive values: objects are keyed by identity for
    for_each, and a sensitive list cannot provide keys. Create Secrets
    with a native kubernetes_secret_v1 resource instead.
  EOT
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

locals {
  # Instances are keyed by object IDENTITY (kind/namespace/name), never by
  # list position. With index keys, a later run that re-applies this state
  # with an edited list (a changed serviceAccountRef, one object removed,
  # a reordering) turned "the same object, changed" into destroy+create of
  # whatever now sat at each index — HarborAccess CRs were deleted and
  # re-created instead of updated. Identity keys give in-place updates for
  # edits and a real delete only for objects that left the list.
  keyed = {
    for m in var.manifests :
    format("%s/%s/%s",
      yamldecode(m.yaml).kind,
      try(yamldecode(m.yaml).metadata.namespace, ""),
      yamldecode(m.yaml).metadata.name,
    ) => m
  }

  # Split namespaces from everything else so namespaces apply first.
  # kubernetes_manifest with for_each parallelises all instances, which
  # races: SAs / Secrets in a yet-to-exist namespace fail with
  # "namespaces XYZ not found". Two resource blocks with depends_on
  # serialises namespace creation before its tenants (and, on destroy,
  # tenant deletion before the namespace).
  namespaces = { for k, m in local.keyed : k => m if yamldecode(m.yaml).kind == "Namespace" }
  tenants    = { for k, m in local.keyed : k => m if yamldecode(m.yaml).kind != "Namespace" }
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
