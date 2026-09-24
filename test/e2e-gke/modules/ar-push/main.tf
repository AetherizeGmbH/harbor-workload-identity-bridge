# Pushes locally-built working-tree images to the run's Artifact
# Registry repo — the GKE replacement for `kind load docker-image`
# (ADR-0022). Requires docker + gcloud on the machine running the
# harness; `gcloud auth configure-docker` wires the docker credential
# helper for the AR host.
#
# tofu test state is ephemeral (fresh per run, destroyed at the end),
# so each run re-pushes; docker push is layer-deduplicating and cheap
# on re-runs.
terraform {
  required_version = ">= 1.6"
  # null provider is built-in; no external providers needed.
}

variable "images" {
  type        = map(string)
  description = "name → local docker tag (e.g. bridge → \"harbor-bridge:e2e\"). The name becomes the AR image name; the local tag's version part is kept."
}

variable "registry_host" {
  type        = string
  description = "Artifact Registry docker host (e.g. europe-west3-docker.pkg.dev)."
}

variable "target_repo" {
  type        = string
  description = "Full AR repository prefix (host/project/repo) to push under."
}

locals {
  targets = {
    for name, local_tag in var.images :
    name => "${var.target_repo}/${name}:${split(":", local_tag)[1]}"
  }
}

resource "null_resource" "push" {
  for_each = var.images

  triggers = {
    source = each.value
    target = local.targets[each.key]
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -euo pipefail
      gcloud auth configure-docker '${var.registry_host}' --quiet
      docker tag '${each.value}' '${local.targets[each.key]}'
      docker push '${local.targets[each.key]}'
    BASH
  }
}

output "image_refs" {
  description = "name → {repository, tag} in the shape harbor-bridge-install expects."
  value = {
    for name, target in local.targets :
    name => {
      repository = split(":", target)[0]
      tag        = split(":", target)[1]
    }
  }
  depends_on = [null_resource.push]
}

output "image_tags" {
  description = "name → full pushed ref, for direct use as a pod image."
  value       = local.targets
  depends_on  = [null_resource.push]
}
