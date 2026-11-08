# Assertions on the kind nodes themselves: node_script runs in every node
# container (`docker exec <node> sh -c`), cluster_script on the harness host
# with `k` (kubectl on var.kubeconfig only, test/e2e/scripts/kubeconfig.sh).
# Each must exit 0. On failure their output lands in
# test/e2e/.diag/<name>/ (the provisioner's own output is suppressed: its
# environment is sensitive) and the run fails.

terraform {
  required_version = ">= 1.6"
  required_providers {
    null = {
      source  = "hashicorp/null"
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

variable "name" {
  type        = string
  description = "Name of the check: the diagnostics directory under test/e2e/.diag/."
}

variable "node_names" {
  type        = list(string)
  description = "Docker container names of the nodes node_script runs on."
}

variable "node_script" {
  type        = string
  description = "POSIX sh script run in every node container; it must exit 0."
}

variable "cluster_script" {
  type        = string
  default     = ""
  description = "bash script run on the harness host, with k (kubectl on the harness kubeconfig); it must exit 0. Empty skips it."
}

variable "fail_message" {
  type        = string
  description = "What a failure means, written to the diagnostics."
}

resource "null_resource" "check" {
  triggers = {
    name = var.name
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      source "$KUBECONFIG_LIB" && harness_kubeconfig "$d" || exit 1
      rm -rf "$DIAG"; mkdir -p "$DIAG"
      failed=""
      for n in $NODES; do
        if ! docker exec "$n" sh -c "$NODE_SCRIPT" > "$DIAG/node-$n.txt" 2>&1; then
          failed+=" $n"
        fi
      done
      if [ -n "$CLUSTER_SCRIPT" ]; then
        export HARNESS_KUBECONFIG
        export -f k
        if ! bash -c "$CLUSTER_SCRIPT" > "$DIAG/cluster.txt" 2>&1; then
          failed+=" cluster"
        fi
      fi
      if [ -n "$failed" ]; then
        echo "$FAIL_MESSAGE (failed on:$failed)" > "$DIAG/FAILED"
        echo "FAILED: $FAIL_MESSAGE (failed on:$failed; diagnostics: $DIAG)" >&2
        exit 1
      fi
      rm -rf "$DIAG"
    BASH
    environment = {
      K8S_HOST       = var.kubeconfig.host
      K8S_CA         = var.kubeconfig.cluster_ca_certificate
      K8S_CERT       = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
      K8S_KEY        = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
      K8S_TOKEN      = var.kubeconfig.token == null ? "" : var.kubeconfig.token
      KUBECONFIG_LIB = abspath("${path.module}/../../scripts/kubeconfig.sh")
      NODES          = join(" ", var.node_names)
      NODE_SCRIPT    = var.node_script
      CLUSTER_SCRIPT = var.cluster_script
      FAIL_MESSAGE   = var.fail_message
      DIAG           = abspath("${path.cwd}/.diag/${var.name}")
    }
  }
}
