terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
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
  type        = string
  default     = "test-exec-pod failed; check pod logs"
  description = "Printed and written to the diagnostics directory when the Job does not complete."
}

variable "diag_dir" {
  type        = string
  default     = null
  description = "Where diagnostics land when the Job fails. Default: <cwd>/.diag/<name>, i.e. test/e2e/.diag/ (gitignored; CI uploads it)."
}

variable "diag_bridge_namespace" {
  type        = string
  default     = "harbor-bridge-system"
  description = "Namespace of the bridge release whose logs and HarborAccess state are captured on failure."
}

variable "node_log_command" {
  type        = string
  default     = ""
  description = "Optional shell command printing the kubelet log of the node the pod ran on; {node} is replaced with the node name. The kind harness uses `docker exec {node} journalctl -u kubelet`. Empty (GKE) skips it."
}

variable "expect_pull_failure" {
  type        = bool
  default     = false
  description = "Invert the assertion: the run passes only when kubelet reports an AUTHORIZATION failure pulling var.image (401/403/unauthorized/denied), and fails if the Job succeeds. A 'not found' failure does not count — it would mean a broken setup, not a revoked identity."
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
  token                  = var.kubeconfig.token
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
  # Completion is awaited by null_resource.wait below, which can capture
  # diagnostics before failing. With wait_for_completion = true a failed
  # pull only surfaced as "job ... is not in complete state" and the
  # evidence was destroyed with the cluster at the end of the test.
  wait_for_completion = false
}

locals {
  diag_dir = coalesce(var.diag_dir, abspath("${path.cwd}/.diag/${var.name}"))
}

# Waits for the Job to succeed. On failure or timeout it writes pod,
# event, bridge, plugin and (optionally) kubelet diagnostics to diag_dir
# and fails the run. Secret CONTENTS are never captured — only names.
# kubectl is driven ONLY by var.kubeconfig (--kubeconfig=/dev/null keeps
# the operator's ~/.kube/config out of it). Output of this provisioner is
# suppressed by OpenTofu (sensitive environment), hence the files.
resource "null_resource" "wait" {
  triggers = {
    job_uid = kubernetes_job_v1.this.metadata[0].uid
  }
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      chmod 700 "$d"
      printf '%s' "$K8S_CA" > "$d/ca.crt"
      args=(--kubeconfig=/dev/null --server="$K8S_HOST" --certificate-authority="$d/ca.crt")
      if [ -n "$K8S_TOKEN" ]; then
        args+=(--token="$K8S_TOKEN")
      else
        printf '%s' "$K8S_CERT" > "$d/tls.crt"
        printf '%s' "$K8S_KEY" > "$d/tls.key"
        args+=(--client-certificate="$d/tls.crt" --client-key="$d/tls.key")
      fi
      k() { kubectl "$${args[@]}" "$@"; }

      deadline=$(( $(date +%s) + TIMEOUT ))
      while [ "$(date +%s)" -lt "$deadline" ]; do
        ok="$(k -n "$NS" get job "$JOB" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)"
        failed="$(k -n "$NS" get job "$JOB" -o jsonpath='{.status.failed}' 2>/dev/null || true)"
        if [ "$EXPECT_PULL_FAILURE" = "true" ]; then
          if [ "$${ok:-0}" -ge 1 ]; then
            FAIL_MESSAGE="pull unexpectedly SUCCEEDED (expected an authorization failure): $FAIL_MESSAGE"
            break
          fi
          pod="$(k -n "$NS" get pod -l "app=$JOB" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
          if [ -n "$pod" ]; then
            msgs="$(k -n "$NS" get events --field-selector "involvedObject.name=$pod,reason=Failed" -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)"
            if printf '%s' "$msgs" | grep -q 'Failed to pull image'; then
              if printf '%s' "$msgs" | grep -Eqi '401|403|unauthori[sz]ed|forbidden|denied|insufficient_scope'; then
                exit 0
              fi
              FAIL_MESSAGE="pull failed, but not with an authorization error: $FAIL_MESSAGE"
              break
            fi
          fi
        else
          if [ "$${ok:-0}" -ge 1 ]; then exit 0; fi
        fi
        if [ "$${failed:-0}" -ge 1 ]; then break; fi
        sleep 3
      done

      rm -rf "$DIAG"; mkdir -p "$DIAG"
      echo "$FAIL_MESSAGE" > "$DIAG/FAILED"
      k -n "$NS" describe job "$JOB" > "$DIAG/job.describe.txt" 2>&1
      k -n "$NS" describe pod -l "app=$JOB" > "$DIAG/pod.describe.txt" 2>&1
      k -n "$NS" logs -l "app=$JOB" --all-containers --tail=-1 > "$DIAG/pod.log" 2>&1
      k -n "$NS" get events --sort-by=.lastTimestamp > "$DIAG/events.txt" 2>&1
      k -n "$BRIDGE_NS" logs -l app.kubernetes.io/component=bridge --all-containers --tail=1000 --prefix > "$DIAG/bridge.log" 2>&1
      k get harboraccesses -A -o yaml > "$DIAG/harboraccesses.yaml" 2>&1
      k -n "$BRIDGE_NS" get secrets -o name > "$DIAG/bridge-secret-names.txt" 2>&1
      node="$(k -n "$NS" get pod -l "app=$JOB" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
      echo "$node" > "$DIAG/node.txt"
      if [ -n "$node" ]; then
        plugin_pod="$(k -n "$BRIDGE_NS" get pod -l app.kubernetes.io/component=plugin --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
        if [ -n "$plugin_pod" ]; then
          k -n "$BRIDGE_NS" logs "$plugin_pod" --all-containers --tail=-1 > "$DIAG/plugin-installer.log" 2>&1
        fi
        if [ -n "$NODE_LOG_COMMAND" ]; then
          bash -c "$${NODE_LOG_COMMAND//\{node\}/$node}" > "$DIAG/kubelet.log" 2>&1
        fi
      fi
      echo "FAILED: $FAIL_MESSAGE (diagnostics: $DIAG)" >&2
      exit 1
    BASH
    environment = {
      K8S_HOST            = var.kubeconfig.host
      K8S_CA              = var.kubeconfig.cluster_ca_certificate
      K8S_CERT            = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
      K8S_KEY             = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
      K8S_TOKEN           = var.kubeconfig.token == null ? "" : var.kubeconfig.token
      NS                  = local.ns
      JOB                 = var.name
      TIMEOUT             = tostring(var.timeout_seconds)
      DIAG                = local.diag_dir
      BRIDGE_NS           = var.diag_bridge_namespace
      FAIL_MESSAGE        = var.fail_message
      NODE_LOG_COMMAND    = var.node_log_command
      EXPECT_PULL_FAILURE = tostring(var.expect_pull_failure)
    }
  }
}
