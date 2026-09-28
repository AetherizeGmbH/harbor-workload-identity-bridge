# Runs a bash check against the cluster from the host, for the stages
# that assert or change Kubernetes state rather than pull an image:
# conditions and Secret annotations the bridge writes, and the Nexus
# outage (scale to zero, delete, scale back).
#
# The script runs with
#   k                  kubectl with the harness kubeconfig ONLY
#                      (--kubeconfig=<kubeconfig_path>; never the
#                      operator's ~/.kube/config or its context)
#   fail MESSAGE       print MESSAGE and fail the check
#   retry SECONDS CMD  run CMD every 3 s until it succeeds; non-zero
#                      after SECONDS
#   STATE_DIR          a directory under test/e2e/.gen/ that survives
#                      between runs, for values a later check compares
#                      (object UIDs, user ids; never a credential)
# plus var.environment. It must print no credential: its output is
# written to the diagnostics directory on failure and shows in the
# `tofu test` log (the kubeconfig travels as a file path, so OpenTofu
# does not suppress the provisioner's output).
#
# On timeout the check's process group gets SIGTERM, which the check
# sees as an exit: an EXIT trap it set (e.g. to undo an outage) runs.
#
# Every run re-runs the check (the trigger is a fresh uuid per plan), so
# runs that share this module's state each execute their own script.
terraform {
  required_version = ">= 1.6"
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.0"
    }
  }
}

variable "kubeconfig_path" {
  type        = string
  description = "The kubeconfig file the harness wrote (kind-cluster's kubeconfig_path output)."
}

variable "name" {
  type        = string
  description = "Name of the check; the diagnostics land in .diag/<name>."

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]*$", var.name))
    error_message = "name must be lower-case letters, digits and '-'."
  }
}

variable "script" {
  type        = string
  description = "The bash check. set -euo pipefail is in effect."
}

variable "timeout_seconds" {
  type    = number
  default = 300
}

variable "fail_message" {
  type    = string
  default = "kubectl check failed"
}

variable "environment" {
  type        = map(string)
  default     = {}
  description = "Extra environment variables for the script. Never a credential."
}

variable "diag_dir" {
  type        = string
  default     = null
  description = "Where diagnostics land on failure. Default: <cwd>/.diag/<name>."
}

variable "diag_bridge_namespace" {
  type    = string
  default = "harbor-bridge-system"
}

variable "state_dir" {
  type        = string
  default     = null
  description = "Directory for values later checks compare. Default: <cwd>/.gen/e2e-state."
}

locals {
  diag_dir  = coalesce(var.diag_dir, abspath("${path.cwd}/.diag/${var.name}"))
  state_dir = coalesce(var.state_dir, abspath("${path.cwd}/.gen/e2e-state"))
}

resource "null_resource" "check" {
  triggers = {
    run = uuid()
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      work="$(mktemp -d)"
      trap 'rm -rf "$work"' EXIT
      mkdir -p "$STATE_DIR"
      printf '%s\n' "$SCRIPT" > "$work/check.sh"

      k() { kubectl --kubeconfig="$KUBECONFIG_PATH" "$@"; }
      fail() { echo "FAIL: $*" >&2; exit 1; }
      retry() {
        local deadline=$(( $(date +%s) + $1 )); shift
        until "$@"; do
          [ "$(date +%s)" -lt "$deadline" ] || return 1
          sleep 3
        done
      }

      # The check runs in a subshell (it inherits k, fail and retry) with
      # a watchdog; macOS has no timeout(1). Job control puts the subshell
      # in a process group of its own, so the watchdog stops it together
      # with whatever it is waiting for, and a TERM becomes an exit that
      # runs the check's EXIT trap. Neither may hold the provisioner's
      # output open once the check ended, or OpenTofu would wait for them:
      # the check writes to a file, and the watchdog polls and exits within
      # a second of the check.
      set -m
      ( set -euo pipefail; trap 'exit 143' TERM; . "$work/check.sh" ) > "$work/check.log" 2>&1 &
      pid=$!
      set +m
      (
        end=$(( $(date +%s) + TIMEOUT ))
        while kill -0 "$pid" 2>/dev/null; do
          if [ "$(date +%s)" -ge "$end" ]; then kill -TERM -- "-$pid"; break; fi
          sleep 1
        done
      ) > /dev/null 2>&1 &
      watchdog=$!
      wait "$pid"; rc=$?
      wait "$watchdog" 2>/dev/null || true
      cat "$work/check.log"
      if [ "$rc" -eq 0 ]; then exit 0; fi

      msg="$FAIL_MESSAGE"
      if [ "$rc" -ge 128 ]; then msg="timed out after $TIMEOUT s: $msg"; fi
      rm -rf "$DIAG"; mkdir -p "$DIAG"
      echo "$msg" > "$DIAG/FAILED"
      cp "$work/check.log" "$DIAG/check.log"
      k -n "$BRIDGE_NS" logs -l app.kubernetes.io/component=bridge --all-containers --tail=1000 --prefix > "$DIAG/bridge.log" 2>&1
      k get nexusaccesses -A -o yaml > "$DIAG/nexusaccesses.yaml" 2>&1
      k get harboraccesses -A -o yaml > "$DIAG/harboraccesses.yaml" 2>&1
      k -n "$BRIDGE_NS" get secrets -o name > "$DIAG/bridge-secret-names.txt" 2>&1
      k get events -A --sort-by=.lastTimestamp > "$DIAG/events.txt" 2>&1
      k get pods -A -o wide > "$DIAG/pods.txt" 2>&1
      echo "FAILED: $msg (diagnostics: $DIAG)" >&2
      exit 1
    BASH
    environment = merge(var.environment, {
      KUBECONFIG_PATH = var.kubeconfig_path
      SCRIPT          = var.script
      TIMEOUT         = tostring(var.timeout_seconds)
      FAIL_MESSAGE    = var.fail_message
      DIAG            = local.diag_dir
      BRIDGE_NS       = var.diag_bridge_namespace
      STATE_DIR       = local.state_dir
    })
  }
}
