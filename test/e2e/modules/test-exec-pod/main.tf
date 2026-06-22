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
    client_certificate     = string
    client_key             = string
    cluster_ca_certificate = string
  })
  sensitive = true
}

variable "name" {
  type        = string
  description = "Name + namespace base for the job"
}

variable "namespace" {
  type        = string
  default     = ""
  description = "Namespace; defaults to var.name."
}

variable "service_account_name" {
  type        = string
  default     = ""
  description = "Existing SA to attach. Empty = the module creates one."
}

variable "node_name" {
  type        = string
  default     = ""
  description = "Pin the pod to a specific node (optional)."
}

variable "image" {
  type        = string
  description = "Container image (uses the credential providers to pull if needed)"
}

variable "command" {
  type    = list(string)
  default = ["sh", "-c"]
}

variable "args" {
  type    = list(string)
  default = ["echo ok && sleep 5"]
}

variable "timeout_seconds" {
  type    = number
  default = 300
}

variable "fail_message" {
  type    = string
  default = "test-exec-pod failed; check pod logs"
}

variable "image_pull_policy" {
  type        = string
  default     = "Always"
  description = "Container imagePullPolicy. Use IfNotPresent for kind-loaded, local-only images (e.g. e2e-seed:e2e) that no registry can serve."
}

variable "env_from_secret" {
  type        = string
  default     = ""
  description = "Optional Secret name in the job namespace; its keys are exposed as env vars via envFrom (e.g. a robot-credential Secret's username/password)."
}

locals {
  ns      = coalesce(var.namespace, var.name)
  use_sa  = var.service_account_name != ""
  sa_name = local.use_sa ? var.service_account_name : "${var.name}-sa"
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

resource "kubernetes_job_v1" "this" {
  metadata {
    name      = var.name
    namespace = local.ns
  }
  spec {
    backoff_limit              = 0
    active_deadline_seconds    = var.timeout_seconds
    ttl_seconds_after_finished = 600
    template {
      metadata { labels = { app = var.name } }
      spec {
        restart_policy       = "Never"
        service_account_name = local.sa_name
        node_name            = var.node_name != "" ? var.node_name : null
        container {
          name              = "main"
          image             = var.image
          image_pull_policy = var.image_pull_policy
          command           = var.command
          args              = var.args
          dynamic "env_from" {
            for_each = var.env_from_secret != "" ? [var.env_from_secret] : []
            content {
              secret_ref {
                name = env_from.value
              }
            }
          }
        }
      }
    }
  }
  wait_for_completion = true
  timeouts {
    create = "${var.timeout_seconds}s"
  }
}
