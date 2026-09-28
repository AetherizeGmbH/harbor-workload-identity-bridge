# The NexusAccess fixture set of the Nexus e2e run
# (tests/03-nexus.tftest.hcl), rendered for one lifecycle phase. As in
# harbor-access-scenario, every run of this module shares ONE OpenTofu
# state and k8s-yaml keys objects by identity, so moving between phases
# edits, creates and deletes objects the way a user would.
#
#   initial — namespaces, ServiceAccounts, RBAC and these objects:
#               puller  (nx-pull)  nx-pull/puller: pull nx-app, nx-extra;
#                                  plus HarborAccess "puller" in the bridge
#                                  namespace (your-project): one
#                                  ServiceAccount with both backends
#               pusher  (bridge)   nx-ci/pusher: pull,push nx-push
#               editor  (nx-edit)  nx-edit/editor: pull nx-app, nx-extra
#               mover   (nx-move)  nx-move/old-sa: pull nx-app
#   updated — the same objects, edited in place:
#               puller loses nx-extra (a grant removal must revoke it in
#                 Nexus without rotating the password);
#               editor loses nx-extra (repository A) and names the missing
#                 nx-missing (repository C): RepositoryNotFound, A revoked
#                 at once (ADR-0036 decision d). Its generation is never
#                 observed, so the phase does not wait for it;
#               mover moves to nx-move/new-sa (identity change: the old
#                 identity's user and role go).
#   none    — everything deleted while the bridge runs; each deletion
#             waits for the finalizer.
#
# RBAC, least privilege for the check Jobs:
#   - nx-pull/puller and nx-edit/editor may create tokens for themselves
#     only: the routing and refusal checks run as them and ask the bridge
#     directly with a token bound to their own pod (ADR-0028);
#   - nexus-check (bridge namespace) may get, patch and delete exactly the
#     Secrets of puller and editor: the rotation checks read the new
#     credentials and start the rotations.
#
# nx-outage (ServiceAccount probe) hosts the object the nexus_outage
# check creates and deletes itself.
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

  namespaces = ["nx-pull", "nx-ci", "nx-edit", "nx-move", "nx-outage"]

  service_accounts = concat(
    [
      { namespace = "nx-pull", name = "puller" },
      { namespace = "nx-ci", name = "pusher" },
      { namespace = "nx-edit", name = "editor" },
      { namespace = "nx-move", name = "old-sa" },
      { namespace = "nx-outage", name = "probe" },
      { namespace = var.bridge_namespace, name = "nexus-check" },
    ],
    local.updated ? [{ namespace = "nx-move", name = "new-sa" }] : [],
  )

  # Canonical Go duration: the Duration type re-serialises "1h" as
  # "1h0m0s", which would trip kubernetes_manifest's consistency check.
  token_ttl = "1h0m0s"

  pull = { for r in ["nx-app", "nx-extra", "nx-missing"] : r => { name = r, access = "pull", format = "docker" } }

  nexus_accesses = [
    {
      name         = "puller"
      namespace    = "nx-pull"
      sa           = { namespace = "nx-pull", name = "puller" }
      repositories = local.updated ? [local.pull["nx-app"]] : [local.pull["nx-app"], local.pull["nx-extra"]]
      generation   = local.updated ? 2 : 1
    },
    {
      name         = "pusher"
      namespace    = var.bridge_namespace
      sa           = { namespace = "nx-ci", name = "pusher" }
      repositories = [{ name = "nx-push", access = "pull,push", format = "docker" }]
      generation   = 1
    },
    {
      name         = "editor"
      namespace    = "nx-edit"
      sa           = { namespace = "nx-edit", name = "editor" }
      repositories = local.updated ? [local.pull["nx-app"], local.pull["nx-missing"]] : [local.pull["nx-app"], local.pull["nx-extra"]]
      # RepositoryNotFound never advances observedGeneration: no wait.
      generation = local.updated ? null : 1
    },
    {
      name         = "mover"
      namespace    = "nx-move"
      sa           = { namespace = "nx-move", name = local.updated ? "new-sa" : "old-sa" }
      repositories = [local.pull["nx-app"]]
      generation   = local.updated ? 2 : 1
    },
  ]

  harbor_accesses = [
    {
      name        = "puller"
      namespace   = var.bridge_namespace
      sa          = { namespace = "nx-pull", name = "puller" }
      permissions = [{ project = "your-project", action = "pull" }]
    },
  ]

  # A ServiceAccount that may create tokens for itself and nothing else.
  self_token_rbac = flatten([
    for sa in [{ namespace = "nx-pull", name = "puller" }, { namespace = "nx-edit", name = "editor" }] : [
      {
        apiVersion = "rbac.authorization.k8s.io/v1"
        kind       = "Role"
        metadata   = { name = "mint-own-token", namespace = sa.namespace }
        rules = [{
          apiGroups     = [""]
          resources     = ["serviceaccounts/token"]
          resourceNames = [sa.name]
          verbs         = ["create"]
        }]
      },
      {
        apiVersion = "rbac.authorization.k8s.io/v1"
        kind       = "RoleBinding"
        metadata   = { name = "mint-own-token", namespace = sa.namespace }
        roleRef    = { apiGroup = "rbac.authorization.k8s.io", kind = "Role", name = "mint-own-token" }
        subjects   = [{ kind = "ServiceAccount", name = sa.name, namespace = sa.namespace }]
      },
    ]
  ])

  check_rbac = [
    {
      apiVersion = "rbac.authorization.k8s.io/v1"
      kind       = "Role"
      metadata   = { name = "nexus-check", namespace = var.bridge_namespace }
      rules = [{
        apiGroups     = [""]
        resources     = ["secrets"]
        resourceNames = ["nexususer-nx-pull.puller", "nexususer-nx-edit.editor"]
        verbs         = ["get", "patch", "delete"]
      }]
    },
    {
      apiVersion = "rbac.authorization.k8s.io/v1"
      kind       = "RoleBinding"
      metadata   = { name = "nexus-check", namespace = var.bridge_namespace }
      roleRef    = { apiGroup = "rbac.authorization.k8s.io", kind = "Role", name = "nexus-check" }
      subjects   = [{ kind = "ServiceAccount", name = "nexus-check", namespace = var.bridge_namespace }]
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
    [for o in concat(local.self_token_rbac, local.check_rbac) : {
      yaml = yamlencode(o)
      wait = null
    }],
    [for nxa in local.nexus_accesses : {
      yaml = yamlencode({
        apiVersion = "nexus.aetherize.io/v1alpha1"
        kind       = "NexusAccess"
        metadata   = { name = nxa.name, namespace = nxa.namespace }
        spec = {
          serviceAccountRef = nxa.sa
          trustPolicy       = { issuer = var.issuer, audience = var.audience }
          repositories      = nxa.repositories
          tokenTTL          = local.token_ttl
        }
      })
      # The bridge advances status.observedGeneration only at the end of a
      # fully successful pass, so reaching the generation means Ready at
      # exactly this spec (ADR-0023).
      wait = nxa.generation == null ? null : {
        fields = { "status.observedGeneration" = "^${nxa.generation}$" }
      }
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
          tokenTTL          = local.token_ttl
        }
      })
      wait = {
        fields = { "status.observedGeneration" = "^1$" }
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

output "nexus_accesses" {
  value       = var.phase == "none" ? [] : [for nxa in local.nexus_accesses : "${nxa.namespace}/${nxa.name}"]
  description = "NexusAccess objects present in this phase."
}
