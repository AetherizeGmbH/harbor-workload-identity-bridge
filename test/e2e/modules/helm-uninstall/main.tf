# `helm uninstall` of a release, with its hooks, from the harness host
# (ADR-0039: the chart's pre-delete hook has every node remove the plugin
# before Helm deletes anything).
#
# The release was installed through the helm provider (harbor-bridge-install),
# whose state still holds it afterwards. That is fine: the provider drops a
# release that no longer exists when it refreshes, and its delete returns
# without uninstalling one that is gone, so the cleanup at the end of
# `tofu test` has nothing to do for it.
#
# On failure the provisioner saves the helm output, the hook's log, the
# DaemonSet's pods and their installer logs, and each node's kubelet
# journal to diag_dir, and then uninstalls with --no-hooks, so that the
# cleanup does not run into a half-removed release. kubectl and helm see
# only var.kubeconfig, through a private file (test/e2e/scripts/kubeconfig.sh).

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
    # Client-cert auth (kind) and token auth (GKE) are alternatives;
    # exactly one pair/field is expected to be set.
    client_certificate = optional(string)
    client_key         = optional(string)
    token              = optional(string)
  })
  sensitive = true
}

variable "release" {
  type        = string
  description = "Helm release to uninstall."
}

variable "namespace" {
  type        = string
  description = "Namespace of the release, and of its plugin DaemonSet (the harness leaves plugin.namespace empty)."
}

variable "timeout" {
  type        = string
  default     = "6m"
  description = "helm uninstall --timeout: above the chart's plugin.uninstall.timeoutSeconds (270s), so that the hook, not Helm, reports a node that did not finish."
}

variable "node_names" {
  type        = list(string)
  default     = []
  description = "Docker container names of the kind nodes, for the kubelet journals on failure."
}

variable "diag_name" {
  type        = string
  default     = ""
  description = "Directory name under test/e2e/.diag/ for failure diagnostics. Empty: <release>-uninstall."
}

resource "null_resource" "uninstall" {
  triggers = {
    release   = var.release
    namespace = var.namespace
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      source "$KUBECONFIG_LIB" && harness_kubeconfig "$d" || exit 1
      rm -rf "$DIAG"; mkdir -p "$DIAG"
      # How long helm uninstall takes, hook included, against the chart's
      # plugin.uninstall.timeoutSeconds (270 s): in the CI job summary
      # (GITHUB_STEP_SUMMARY, when set), in $TIMINGS (CI uploads it from
      # every run) and in the diagnostics.
      timing() {
        local line="helm uninstall $RELEASE: $1 after $(( SECONDS - start )) s ($(wc -w <<<"$NODES" | tr -d ' ') nodes)"
        echo "$line" > "$DIAG/timing.txt"
        mkdir -p "$(dirname "$TIMINGS")" && echo "$line" >> "$TIMINGS"
        if [ -n "$${GITHUB_STEP_SUMMARY:-}" ]; then echo "- $line" >> "$GITHUB_STEP_SUMMARY"; fi
      }
      start=$SECONDS
      if helm --kubeconfig "$HARNESS_KUBECONFIG" uninstall "$RELEASE" --namespace "$NS" --timeout "$TIMEOUT" > "$DIAG/helm-uninstall.log" 2>&1; then
        timing succeeded
        rm -rf "$DIAG"
        exit 0
      fi
      timing failed
      echo "helm uninstall $RELEASE failed; see helm-uninstall.log and hook.log" > "$DIAG/FAILED"
      k -n "$NS" get daemonsets,pods,jobs -o wide > "$DIAG/objects.txt" 2>&1
      k -n "$NS" get pods -o yaml > "$DIAG/pods.yaml" 2>&1
      k -n "$NS" logs -l app.kubernetes.io/component=plugin-uninstall --tail=2000 > "$DIAG/hook.log" 2>&1
      k -n "$NS" logs -l app.kubernetes.io/component=plugin -c install --tail=1000 --prefix > "$DIAG/install.log" 2>&1
      for n in $NODES; do
        docker exec "$n" journalctl -u kubelet --no-pager --since -20min > "$DIAG/kubelet-$n.log" 2>&1
      done
      helm --kubeconfig "$HARNESS_KUBECONFIG" uninstall "$RELEASE" --namespace "$NS" --no-hooks --ignore-not-found > "$DIAG/helm-uninstall-no-hooks.log" 2>&1
      echo "FAILED: helm uninstall $RELEASE (diagnostics: $DIAG)" >&2
      exit 1
    BASH
    environment = {
      K8S_HOST       = var.kubeconfig.host
      K8S_CA         = var.kubeconfig.cluster_ca_certificate
      K8S_CERT       = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
      K8S_KEY        = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
      K8S_TOKEN      = var.kubeconfig.token == null ? "" : var.kubeconfig.token
      KUBECONFIG_LIB = abspath("${path.module}/../../scripts/kubeconfig.sh")
      RELEASE        = var.release
      NS             = var.namespace
      TIMEOUT        = var.timeout
      NODES          = join(" ", var.node_names)
      DIAG           = abspath("${path.cwd}/.diag/${var.diag_name != "" ? var.diag_name : "${var.release}-uninstall"}")
      TIMINGS        = abspath("${path.cwd}/.timings/uninstall.txt")
    }
  }
}
