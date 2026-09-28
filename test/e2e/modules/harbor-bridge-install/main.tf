terraform {
  required_version = ">= 1.6"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
    kubectl = {
      source  = "alekc/kubectl"
      version = "~> 2.1"
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

variable "namespace" {
  type    = string
  default = "harbor-bridge-system"
}

variable "cluster_name" {
  type        = string
  description = "BRIDGE_CLUSTER_NAME — used as robot-account prefix in Harbor."
}

variable "harbor_url" {
  type        = string
  description = "In-cluster Harbor REST API URL (e.g. http://harbor-core.harbor.svc.cluster.local). The bridge talks to this directly over service DNS."
}

variable "harbor_admin_password" {
  type      = string
  sensitive = true
}

variable "audience" {
  type        = string
  description = "Audience the plugin's tokenAttributes uses. Must match HarborAccess.trustPolicy.audience."
}

variable "match_images" {
  type        = list(string)
  description = "kubelet matchImages glob patterns (full registry host:port form)."
}

variable "install_mode" {
  type        = string
  default     = "auto"
  description = "plugin.install.mode (ADR-0021): auto | merge | patch | none. On kind, auto resolves to patch (no pre-wired kubelet flags); on GKE it resolves to merge."

  validation {
    condition     = contains(["auto", "merge", "patch", "none"], var.install_mode)
    error_message = "install_mode must be one of auto, merge, patch, none."
  }
}

variable "bridge_endpoint" {
  type        = string
  default     = ""
  description = "plugin.bridgeEndpoint override — escape hatch for dataplanes where the loopback NodePort doesn't route (risk R4, ADR-0022); supports the literal $(NODE_IP). Empty keeps the chart default https://127.0.0.1:<service.nodePort>."
}

variable "chart_path" {
  type        = string
  default     = "../../../../charts/harbor-bridge"
  description = <<-EOT
    Relative to this module dir (test/e2e/modules/harbor-bridge-install).
    Four parents up reaches the repo root, then charts/harbor-bridge.
  EOT
}

variable "bridge_image" {
  type = object({
    repository = string
    tag        = string
  })
  description = "Bridge container image. Required. Caller is expected to derive this from the test's docker-build run output so the install exercises the working-tree Dockerfiles instead of a stale tag."
}

variable "plugin_image" {
  type = object({
    repository = string
    tag        = string
  })
  description = "Plugin container image. Required. See bridge_image."
}

variable "oidc_issuer" {
  type        = string
  default     = "https://kubernetes.default.svc.cluster.local"
  description = "bridge.oidcIssuer — the cluster's ServiceAccount token issuer (kind: the in-cluster default; GKE: the container.googleapis.com URL)."
}

variable "oidc_jwks_url" {
  type        = string
  default     = ""
  description = "bridge.oidcJWKSURL — set to https://kubernetes.default.svc/openid/v1/jwks when the issuer is not the in-cluster apiserver."
}

variable "token_command" {
  type        = string
  default     = ""
  description = "Shell command printing a fresh API token for the teardown guard (GKE: `gcloud auth print-access-token`). Empty = use the kubeconfig credentials captured at install."
}

variable "bridge_replicas" {
  type        = number
  default     = 2
  description = "Bridge replicas. Default 2 like the chart: with one replica the e2e could never catch a data plane that serves only on the leader (audit H1)."
}

variable "issuer_name" {
  type        = string
  default     = "harbor-bridge-ca"
  description = "Name of the self-signed cert-manager ClusterIssuer created for the bridge's serving cert. Referenced by both the ClusterIssuer manifest and the chart's tls.issuerRef."
}

# Resource requests/limits for the bridge Deployment container, wired into
# the chart's bridge.resources. Defaults mirror the chart's own defaults
# (Burstable QoS: limits.memory ≫ requests.memory). Memory must be in Mi or
# Gi format (validated below); CPU is free-form.
variable "bridge_resources" {
  type = object({
    requests = optional(object({
      cpu    = optional(string, "50m")
      memory = optional(string, "64Mi")
    }), {})
    limits = optional(object({
      cpu    = optional(string, "500m")
      memory = optional(string, "256Mi")
    }), {})
  })
  default     = {}
  nullable    = false
  description = "Resource requests/limits for the bridge Deployment container, fed straight into the chart's bridge.resources. Memory must be in Mi or Gi format."

  validation {
    condition = alltrue([
      for m in [var.bridge_resources.requests.memory, var.bridge_resources.limits.memory] :
      can(regex("^[0-9]+(Mi|Gi)$", m))
    ])
    error_message = "requests.memory and limits.memory must be in Mi or Gi format (e.g. \"64Mi\" or \"1Gi\")."
  }
}

variable "harbor_registry_hosts" {
  type        = list(string)
  default     = []
  description = "harbor.registryHosts: the host[:port][/path] entries that route an image to Harbor (BRIDGE_HARBOR_REGISTRY_HOSTS). Empty leaves the value out, and the chart defaults to the host of harbor.url."
}

# The Nexus backend (ADR-0036). null leaves every nexus.* value out of the
# release, so a Harbor-only install renders exactly as before. The
# credentials are separate variables: a sensitive attribute inside this
# object would make `var.nexus == null` sensitive, which count refuses.
variable "nexus" {
  type = object({
    url            = string
    registry_hosts = list(string)
  })
  default     = null
  description = "Enables the Nexus backend: nexus.url (in-cluster REST base URL, without /service/rest) and nexus.registryHosts (host[:port][/path] entries routed to Nexus; each must be covered by match_images)."

  validation {
    condition     = var.nexus == null || (can(regex("^https?://", var.nexus.url)) && length(var.nexus.registry_hosts) > 0)
    error_message = "nexus needs an http(s) url and at least one registry host."
  }
}

variable "nexus_admin_username" {
  type        = string
  default     = null
  sensitive   = true
  description = "The Nexus user the bridge authenticates as, stored in the nexus-admin Secret. Required with nexus. ADR-0036 decision j: a random name, never admin."

  validation {
    condition     = var.nexus == null || (var.nexus_admin_username != null && var.nexus_admin_username != "")
    error_message = "nexus_admin_username is required when nexus is set."
  }
}

variable "nexus_admin_password" {
  type        = string
  default     = null
  sensitive   = true
  description = "Password of nexus_admin_username. Required with nexus."

  validation {
    condition     = var.nexus == null || (var.nexus_admin_password != null && var.nexus_admin_password != "")
    error_message = "nexus_admin_password is required when nexus is set."
  }
}

provider "helm" {
  kubernetes = {
    host                   = var.kubeconfig.host
    client_certificate     = var.kubeconfig.client_certificate
    client_key             = var.kubeconfig.client_key
    token                  = var.kubeconfig.token
    cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  }
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

provider "kubectl" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
  load_config_file       = false
}

resource "kubernetes_namespace_v1" "this" {
  metadata { name = var.namespace }
}

resource "kubernetes_secret_v1" "admin" {
  metadata {
    name      = "harbor-admin"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = {
    username = "admin"
    password = var.harbor_admin_password
  }
  type = "Opaque"
}

resource "kubernetes_secret_v1" "nexus_admin" {
  count = var.nexus == null ? 0 : 1
  metadata {
    name      = "nexus-admin"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = {
    username = var.nexus_admin_username
    password = var.nexus_admin_password
  }
  type = "Opaque"
}

resource "kubectl_manifest" "cluster_issuer" {
  yaml_body         = <<-YAML
    apiVersion: cert-manager.io/v1
    kind: ClusterIssuer
    metadata:
      name: ${var.issuer_name}
    spec:
      selfSigned: {}
  YAML
  server_side_apply = true
  field_manager     = "tofu-e2e-harbor-bridge-install"
}

locals {
  harbor_values = {
    url = var.harbor_url
    # The test Harbor is reached over the pod network (harbor-core
    # Service, plain http); production uses https.
    allowInsecureHTTP = startswith(var.harbor_url, "http://")
    adminCredsSecret = {
      name = kubernetes_secret_v1.admin.metadata[0].name
    }
  }

  # The release's values without the Nexus backend: exactly what a
  # Harbor-only install always rendered.
  base_values = {
    clusterName = var.cluster_name
    harbor      = local.harbor_values
    plugin = merge(
      {
        matchImages = var.match_images
        audience    = var.audience
        install = {
          mode = var.install_mode
        }
        image = var.plugin_image
      },
      var.bridge_endpoint != "" ? { bridgeEndpoint = var.bridge_endpoint } : {},
    )
    bridge = {
      oidcIssuer  = var.oidc_issuer
      oidcJWKSURL = var.oidc_jwks_url
      replicas    = var.bridge_replicas
      logLevel    = "debug"
      image       = var.bridge_image
      resources   = var.bridge_resources
    }
    tls = {
      enabled = true
      issuerRef = {
        name = var.issuer_name
        kind = "ClusterIssuer"
      }
    }
  }

  nexus_values = var.nexus == null ? null : {
    enabled = true
    url     = var.nexus.url
    # The test Nexus is reached over the pod network (plain http), like
    # Harbor above.
    allowInsecureHTTP = startswith(var.nexus.url, "http://")
    adminCredsSecret = {
      name = kubernetes_secret_v1.nexus_admin[0].metadata[0].name
    }
    registryHosts = var.nexus.registry_hosts
  }

  # Keys added to base_values only when set. jsondecode of a string on both
  # branches: the two object shapes cannot unify in a conditional.
  extra_values = merge(
    jsondecode(local.nexus_values == null ? "{}" : jsonencode({ nexus = local.nexus_values })),
    jsondecode(length(var.harbor_registry_hosts) == 0 ? "{}" : jsonencode({ harbor = merge(local.harbor_values, { registryHosts = var.harbor_registry_hosts }) })),
  )
}

# CRDs are installed automatically by helm from the chart's crds/
# directory on first install. No separate kubectl_manifest needed.

resource "helm_release" "bridge" {
  name      = "harbor-bridge"
  namespace = kubernetes_namespace_v1.this.metadata[0].name
  chart     = abspath("${path.module}/${var.chart_path}")
  timeout   = 600
  wait      = true
  atomic    = false # the plugin DaemonSet restarts kubelet — be patient

  values = [length(keys(local.extra_values)) == 0 ? yamlencode(local.base_values) : yamlencode(merge(local.base_values, local.extra_values))]

  depends_on = [
    kubectl_manifest.cluster_issuer,
    kubernetes_secret_v1.nexus_admin,
  ]
}

output "namespace" {
  value = kubernetes_namespace_v1.this.metadata[0].name
}

# The credential endpoint through the bridge's Service, for in-cluster
# checks that call the bridge directly (the plugin uses the NodePort). The
# Service carries the release name (the chart's fullname) and the chart's
# default service.port 8443, which this module does not override; the
# serving certificate names this host.
output "credentials_url" {
  value = "https://${helm_release.bridge.name}.${helm_release.bridge.namespace}.svc:8443/v1/credentials"
}

# Teardown guard. HarborAccess and NexusAccess objects carry the bridge's
# finalizers, which only a running bridge releases. tofu test destroys
# states in reverse order of the LAST run that touched each, so after a
# failure (or with any stage order that ends on an install/upgrade run)
# the bridge used to be uninstalled while HarborAccess objects still
# existed — their deletion, and that of their namespaces, then hung until
# the cleanup timed out.
#
# This resource depends on the release, so tofu destroys it FIRST: its
# destroy-time provisioner deletes every HarborAccess and NexusAccess
# while the bridge is still up (the finalizers revoke the robots and the
# Nexus users, as they would for a user), and only then does helm
# uninstall the chart. If the bridge cannot release them in time (e.g.
# Harbor or Nexus already gone), the finalizers are dropped so the
# throwaway cluster can be torn down — the test run is ending anyway, and
# a real deletion path is asserted by the scenario's teardown stage while
# everything is healthy.
#
# Ordering comes from depends_on, NOT from a trigger on the release: a
# trigger that is unknown while helm plans an in-place upgrade replaced
# this resource mid-run and its destroy step deleted every HarborAccess
# during bridge_upgrade. For the same reason trigger changes are ignored
# (the GKE access token differs on every plan); the destroy step refreshes
# a token through token_command when one is configured.
resource "null_resource" "release_harboraccess_finalizers" {
  depends_on = [helm_release.bridge]

  lifecycle {
    ignore_changes = [triggers]
  }

  triggers = {
    token_command = var.token_command
    k8s_host      = var.kubeconfig.host
    k8s_ca        = var.kubeconfig.cluster_ca_certificate
    k8s_cert      = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
    k8s_key       = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
    k8s_token     = var.kubeconfig.token == null ? "" : var.kubeconfig.token
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      chmod 700 "$d"
      printf '%s' "$K8S_CA" > "$d/ca.crt"
      args=(--kubeconfig=/dev/null --server="$K8S_HOST" --certificate-authority="$d/ca.crt")
      if [ -n "$TOKEN_COMMAND" ]; then
        K8S_TOKEN="$(bash -c "$TOKEN_COMMAND")"
      fi
      if [ -n "$K8S_TOKEN" ]; then
        args+=(--token="$K8S_TOKEN")
      else
        printf '%s' "$K8S_CERT" > "$d/tls.crt"
        printf '%s' "$K8S_KEY" > "$d/tls.key"
        args+=(--client-certificate="$d/tls.crt" --client-key="$d/tls.key")
      fi
      k() { kubectl "$${args[@]}" "$@"; }
      # release RESOURCE KIND: delete every object of the CRD while the
      # bridge runs; drop the finalizers of what it cannot release in time.
      release() {
        if ! k get crd "$1" >/dev/null 2>&1; then
          echo "no $2 CRD reachable (or no API access); nothing to release" >&2
          return 0
        fi
        if k delete "$1" --all -A --wait=true --timeout=180s; then
          return 0
        fi
        echo "$2 deletion did not finish; dropping finalizers so teardown can proceed" >&2
        for o in $(k get "$1" -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{end}'); do
          k -n "$${o%/*}" patch "$1" "$${o#*/}" --type=merge -p '{"metadata":{"finalizers":null}}' || true
        done
      }
      release harboraccesses.harbor.aetherize.io HarborAccess
      release nexusaccesses.nexus.aetherize.io NexusAccess
      exit 0
    BASH
    environment = {
      K8S_HOST      = self.triggers.k8s_host
      K8S_CA        = self.triggers.k8s_ca
      K8S_CERT      = self.triggers.k8s_cert
      K8S_KEY       = self.triggers.k8s_key
      K8S_TOKEN     = self.triggers.k8s_token
      TOKEN_COMMAND = self.triggers.token_command
    }
  }
}
