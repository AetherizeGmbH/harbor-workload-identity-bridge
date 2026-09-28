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
  description = "Namespace of the bridge release whose logs and HarborAccess state are captured on failure, and whose logs expect_bridge_log searches."
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

variable "env_field_refs" {
  type        = map(string)
  default     = {}
  description = "Optional env vars from the downward API, name → fieldPath (e.g. POD_UID = \"metadata.uid\"), for checks that need their own pod's identity."
}

variable "projected_token_audience" {
  type        = string
  default     = ""
  description = "When set, kubelet projects a token of the pod's ServiceAccount for this audience to /var/run/secrets/tokens/token: bound to the pod, valid for 1h — a token the bridge accepts (ADR-0028), for checks that call the bridge the way the plugin does."
}

variable "expect_bridge_log" {
  type        = list(list(string))
  default     = []
  description = "After the Job succeeded, each entry must match a line of the bridge logs (all replicas, last 15 minutes): every string of the entry occurs on that one line, compared literally. Lets a check assert the bridge's own audit decision, not only the answer the Job saw. Not combinable with expect_pull_failure."

  validation {
    condition = alltrue([
      for e in var.expect_bridge_log : length(e) > 0 && alltrue([for s in e : s != "" && !strcontains(s, "\t") && !strcontains(s, "\n")])
    ])
    error_message = "Each expect_bridge_log entry needs at least one string; the strings must be non-empty and contain no tab or newline."
  }
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
          dynamic "env" {
            for_each = var.env_field_refs
            content {
              name = env.key
              value_from {
                field_ref {
                  field_path = env.value
                }
              }
            }
          }
          dynamic "volume_mount" {
            for_each = var.projected_token_audience != "" ? [1] : []
            content {
              name       = "projected-token"
              mount_path = "/var/run/secrets/tokens"
              read_only  = true
            }
          }
        }
        dynamic "volume" {
          for_each = var.projected_token_audience != "" ? [var.projected_token_audience] : []
          content {
            name = "projected-token"
            projected {
              sources {
                service_account_token {
                  audience           = volume.value
                  expiration_seconds = 3600
                  path               = "token"
                }
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

# Waits for the Job to succeed (and, with expect_bridge_log, for the
# expected bridge log lines). On failure or timeout it writes pod,
# event, bridge, plugin and (optionally) kubelet diagnostics to diag_dir
# and fails the run. Secret CONTENTS are never captured — only names.
# kubectl is driven ONLY by var.kubeconfig, through a private kubeconfig
# file (test/e2e/scripts/kubeconfig.sh): the operator's ~/.kube/config
# stays out of it, and no credential is ever on a command line. Output of
# this provisioner is suppressed by OpenTofu (sensitive environment),
# hence the files.
resource "null_resource" "wait" {
  triggers = {
    job_uid = kubernetes_job_v1.this.metadata[0].uid
  }
  lifecycle {
    precondition {
      condition     = !(var.expect_pull_failure && length(var.expect_bridge_log) > 0)
      error_message = "expect_bridge_log is checked after a successful Job; it cannot be combined with expect_pull_failure."
    }
  }
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      source "$KUBECONFIG_LIB" && harness_kubeconfig "$d" || exit 1

      # Prints the EXPECT_BRIDGE_LOG entries (one per line, their strings
      # tab-separated) that no single bridge log line matches, and fails
      # if there is one. Polls for up to 30s: the bridge writes its audit
      # line before it answers, but the container log can trail a moment.
      bridge_log_missing() {
        local tab=$'\t' logs matched missing entry p i
        local -a parts
        [ -n "$EXPECT_BRIDGE_LOG" ] || return 0
        for i in 1 2 3 4 5 6 7 8 9 10; do
          logs="$(k -n "$BRIDGE_NS" logs -l app.kubernetes.io/component=bridge --all-containers --tail=-1 --since=15m 2>/dev/null || true)"
          missing=""
          while IFS= read -r entry; do
            matched="$logs"
            IFS="$tab" read -r -a parts <<< "$entry"
            for p in "$${parts[@]}"; do
              matched="$(grep -F -- "$p" <<< "$matched" || true)"
            done
            if [ -z "$matched" ]; then missing+="[$${entry//$tab/ + }] "; fi
          done <<< "$EXPECT_BRIDGE_LOG"
          if [ -z "$missing" ]; then return 0; fi
          sleep 3
        done
        printf '%s' "$${missing% }"
        return 1
      }

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
          if [ "$${ok:-0}" -ge 1 ]; then
            if missing="$(bridge_log_missing)"; then exit 0; fi
            FAIL_MESSAGE="the Job succeeded, but no bridge log line matches $missing: $FAIL_MESSAGE"
            break
          fi
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
      KUBECONFIG_LIB      = abspath("${path.module}/../../scripts/kubeconfig.sh")
      NS                  = local.ns
      JOB                 = var.name
      TIMEOUT             = tostring(var.timeout_seconds)
      DIAG                = local.diag_dir
      BRIDGE_NS           = var.diag_bridge_namespace
      FAIL_MESSAGE        = var.fail_message
      NODE_LOG_COMMAND    = var.node_log_command
      EXPECT_PULL_FAILURE = tostring(var.expect_pull_failure)
      EXPECT_BRIDGE_LOG   = join("\n", [for e in var.expect_bridge_log : join("\t", e)])
    }
  }
}
