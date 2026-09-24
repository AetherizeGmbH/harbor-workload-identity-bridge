# The HarborAccess fixture set of the e2e run, rendered for one lifecycle
# phase. Every run that uses this module shares ONE OpenTofu state (tofu
# test keys state by module source), and k8s-yaml keys objects by
# identity, so moving between phases is exactly what a user does:
#
#   initial  — namespaces, ServiceAccounts, HarborAccess CRs (see below).
#   updated  — the same objects, edited in place:
#                * test-access gains pull on project-gamma (grant change →
#                  the robot's permissions must change in Harbor, without a
#                  password rotation that would break kubelet's cache);
#                * collide-one moves from team-a/svc-b to the new SA
#                  team-a/svc-renamed (identity change → a new robot, and
#                  the old robot must be REVOKED in Harbor).
#   none     — everything deleted. Namespaces go too, so app-ns exercises
#              namespace termination blocked on a HarborAccess finalizer.
#              Runs while the bridge is still installed: every finalizer
#              must be released and every robot removed from Harbor.
#
# The wait on each CR is status.observedGeneration equal to the generation
# the phase produces. The bridge sets it only after a fully successful
# pass, so it implies Ready for exactly that spec; waiting on Ready alone
# would pass immediately on the stale Ready=True of the previous phase.
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

variable "phase" {
  type = string
  validation {
    condition     = contains(["initial", "updated", "none"], var.phase)
    error_message = "phase must be initial, updated, or none."
  }
}

variable "bridge_namespace" {
  type = string
}

variable "issuer" {
  type    = string
  default = "https://kubernetes.default.svc.cluster.local"
}

variable "audience" {
  type = string
}

locals {
  updated = var.phase == "updated"

  namespaces = ["test-pull", "team-a", "team", "app-ns", "beta-ns", "upgrade-ns"]

  service_accounts = concat(
    [
      { namespace = "test-pull", name = "image-puller" },
      { namespace = "team-a", name = "svc-b" },
      { namespace = "team", name = "a-svc-b" },
      { namespace = "app-ns", name = "runner" },
      { namespace = "beta-ns", name = "beta-runner" },
      { namespace = "upgrade-ns", name = "upgrade-runner" },
    ],
    local.updated ? [{ namespace = "team-a", name = "svc-renamed" }] : [],
  )

  harbor_accesses = [
    {
      # Baseline identity.
      name        = "test-access"
      namespace   = var.bridge_namespace
      sa          = { namespace = "test-pull", name = "image-puller" }
      permissions = local.updated ? tolist([{ project = "your-project", action = "pull" }, { project = "project-gamma", action = "pull" }]) : tolist([{ project = "your-project", action = "pull" }])
      generation  = local.updated ? 2 : 1
    },
    {
      # ADR-0018 collision pair, half 1: under the old hyphen-joined scheme
      # team-a/svc-b and team/a-svc-b both mapped to bridge-dev-team-a-svc-b.
      name        = "collide-one"
      namespace   = var.bridge_namespace
      sa          = { namespace = "team-a", name = local.updated ? "svc-renamed" : "svc-b" }
      permissions = [{ project = "project-alpha", action = "pull" }]
      generation  = local.updated ? 2 : 1
    },
    {
      # ADR-0018 collision pair, half 2.
      name        = "collide-two"
      namespace   = var.bridge_namespace
      sa          = { namespace = "team", name = "a-svc-b" }
      permissions = [{ project = "project-beta", action = "pull" }]
      generation  = 1
    },
    {
      # Authored in the tenant's own namespace: proves cluster-wide pickup.
      name        = "tenant-access"
      namespace   = "app-ns"
      sa          = { namespace = "app-ns", name = "runner" }
      permissions = [{ project = "project-gamma", action = "pull" }]
      generation  = 1
    },
    {
      # One robot, several projects, pull,push.
      name        = "multi-access"
      namespace   = var.bridge_namespace
      sa          = { namespace = "beta-ns", name = "beta-runner" }
      permissions = [for p in ["beta-1", "beta-2", "beta-3"] : { project = p, action = "pull,push" }]
      generation  = 1
    },
    {
      # Exists from the start; only the kubelet-side matchImages entry is
      # added later (ADR-0021 upgrade convergence).
      name        = "upgrade-access"
      namespace   = var.bridge_namespace
      sa          = { namespace = "upgrade-ns", name = "upgrade-runner" }
      permissions = [{ project = "upgrade-only", action = "pull" }]
      generation  = 1
    },
  ]

  all_manifests = concat(
    [for ns in local.namespaces : {
      yaml = yamlencode({ apiVersion = "v1", kind = "Namespace", metadata = { name = ns } })
      wait = null
    }],
    [for sa in local.service_accounts : {
      yaml = yamlencode({ apiVersion = "v1", kind = "ServiceAccount", metadata = { name = sa.name, namespace = sa.namespace } })
      wait = null
    }],
    [for ha in local.harbor_accesses : {
      yaml = yamlencode({
        apiVersion = "harbor.aetherize.io/v1alpha1"
        kind       = "HarborAccess"
        metadata   = { name = ha.name, namespace = ha.namespace }
        spec = {
          serviceAccountRef = ha.sa
          trustPolicy       = { issuer = var.issuer, audience = var.audience }
          permissions       = ha.permissions
          # Canonical metav1.Duration form: "1h" would be re-serialised to
          # "1h0m0s" and trip kubernetes_manifest's consistency check.
          tokenTTL = "1h0m0s"
        }
      })
      # kubernetes_manifest allows condition OR fields, not both. The
      # bridge advances status.observedGeneration ONLY at the end of a
      # fully successful pass (markReady), so reaching this generation
      # means Ready at exactly this spec.
      wait = {
        fields = { "status.observedGeneration" = "^${ha.generation}$" }
      }
    }],
  )
  # A filter, not a conditional: the two branches of `cond ? [] : [...]`
  # are tuples of different types.
  manifests = [for m in local.all_manifests : m if var.phase != "none"]
}

module "manifests" {
  source     = "../k8s-yaml"
  kubeconfig = var.kubeconfig
  manifests  = local.manifests
}

output "harbor_accesses" {
  value       = var.phase == "none" ? [] : [for ha in local.harbor_accesses : "${ha.namespace}/${ha.name}"]
  description = "HarborAccess objects present in this phase."
}
