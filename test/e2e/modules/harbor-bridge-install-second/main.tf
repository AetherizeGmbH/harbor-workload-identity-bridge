# A second release of the chart on the e2e cluster (ADR-0029), for the
# node-side uninstall stages (ADR-0039): the first release's uninstall must
# keep this release's entry, files and the kubelet flags it needs.
#
# It wraps harbor-bridge-install only to get a state of its own: runs that
# share a module source share one state, and a run with other inputs
# replaces the first release (CLAUDE.md, "tofu test pitfalls").

terraform {
  required_version = ">= 1.6"
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

variable "harbor_url" {
  type = string
}

variable "harbor_admin_password" {
  type      = string
  sensitive = true
}

variable "bridge_image" {
  type = object({
    repository = string
    tag        = string
  })
}

variable "plugin_image" {
  type = object({
    repository = string
    tag        = string
  })
}

variable "name" {
  type        = string
  default     = "harbor-bridge-second"
  description = "Release name, namespace, plugin.providerName, audience and HarborAccess selector value of the second release."
}

variable "cluster_name" {
  type        = string
  default     = "dev2"
  description = "clusterName: bridges that share a Harbor need distinct ones (ADR-0026)."
}

variable "node_port" {
  type    = number
  default = 31444
}

variable "match_images" {
  type    = list(string)
  default = ["harbor.e2e:30843/second-only"]
}

module "install" {
  source = "../harbor-bridge-install"

  kubeconfig             = var.kubeconfig
  namespace              = var.name
  release_name           = var.name
  provider_name          = var.name
  node_port              = var.node_port
  harbor_access_selector = { "harbor.aetherize.io/bridge" = var.name }
  cluster_name           = var.cluster_name
  harbor_url             = var.harbor_url
  harbor_admin_password  = var.harbor_admin_password
  audience               = var.name
  match_images           = var.match_images
  bridge_image           = var.bridge_image
  plugin_image           = var.plugin_image
  bridge_replicas        = 1
  # A named CA ClusterIssuer: the first release runs on the chart's CA for
  # the release (ADR-0042), so this one keeps the tls.issuerRef path covered.
  issuer_name = "${var.name}-ca"
}

output "namespace" {
  value = module.install.namespace
}

output "release" {
  value = module.install.release
}
