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

# A second release on the same cluster (ADR-0029) needs a name, a provider
# name, a NodePort and a HarborAccess selector of its own; the defaults are
# the chart's, for the one release every other run installs.
variable "release_name" {
  type    = string
  default = "harbor-bridge"
}

variable "provider_name" {
  type        = string
  default     = ""
  description = "plugin.providerName; empty keeps the chart's default (harbor-bridge-plugin)."
}

variable "node_port" {
  type        = number
  default     = 0
  description = "service.nodePort; 0 keeps the chart's default (31443)."
}

variable "harbor_access_selector" {
  type        = map(string)
  default     = {}
  description = "bridge.harborAccessSelector; empty serves every HarborAccess."
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
  default     = ""
  description = "bridge.oidcIssuer — the cluster's ServiceAccount token issuer. Empty (the chart default): the bridge reads it from the in-cluster apiserver (ADR-0044)."
}

variable "oidc_jwks_url" {
  type        = string
  default     = ""
  description = "bridge.oidcJWKSURL — with oidc_issuer set to an issuer that is not the in-cluster apiserver, https://kubernetes.default.svc/openid/v1/jwks. Empty with oidc_issuer empty: the apiserver's own key endpoint."
}

variable "token_command" {
  type        = string
  default     = ""
  description = "Shell command printing a fresh API token for the teardown guard (GKE: `gcloud auth print-access-token`). Empty = use the kubeconfig credentials captured at install."
}

variable "bridge_replicas" {
  type        = number
  default     = 2
  description = "Bridge replicas. Default 2 like the chart: with one replica the e2e could never catch a data plane that serves only on the leader (audit H1). The install waits until every replica is Ready (null_resource.bridge_rollout), and the bridge_replicas stage asks each one for credentials directly."
}

variable "critical_pods_quota" {
  type        = bool
  default     = false
  description = "Create a ResourceQuota for PriorityClass system-node-critical pods in the namespace, before the chart. The plugin DaemonSet runs in that class (plugin.priorityClassName default), and GKE admits such pods outside kube-system only in a namespace with a quota for it (docs/platforms.md, GKE): without one the DaemonSet creates no pods and the install times out. kind needs none."
}

variable "issuer_name" {
  type        = string
  default     = ""
  description = "Empty (the default): the chart creates a CA for the release (ADR-0042), which signs the bridge's serving cert and, with mtls, the plugin's client cert; the module sets neither tls.issuerRef nor bridge.mTLS.clientIssuerRef. Otherwise the name of a CA ClusterIssuer this module creates for both certs and names in both values. Its root is <issuer_name>-root, signed by the selfSigned ClusterIssuer <issuer_name>-bootstrap."
}

variable "cert_manager_namespace" {
  type        = string
  default     = "cert-manager"
  description = "Namespace cert-manager runs in: a ClusterIssuer's CA Secret must live in cert-manager's cluster resource namespace, which defaults to it. Used only with issuer_name."
}

variable "mtls" {
  type        = bool
  default     = false
  description = "bridge.mTLS.enabled, with the client certificate issued by the CA that also signs the serving certificate (the release CA, or issuer_name), whose ca.crt the bridge trusts for client certificates."
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

# The bridge's certificates come from a CA, not from a selfSigned issuer.
# With bridge.mTLS the bridge trusts the ca.crt of its own TLS Secret for
# client certificates, so the serving and the plugin's client certificate
# must share an issuing CA (charts/harbor-bridge/templates/
# bridge-certificate.yaml); a selfSigned issuer signs each certificate
# with its own key. By default (issuer_name empty) the chart creates that
# CA for the release (ADR-0042), the path a first install takes. With
# issuer_name, a selfSigned bootstrap issuer signs one root, and the CA
# ClusterIssuer var.issuer_name issues from it: a named, shared issuer.
# Either way every install already has its CA, so turning mTLS on
# (bridge_mtls) changes no CA anywhere. The chart's Certificate waits for
# nothing, so these wait until cert-manager reports each Ready.
resource "kubectl_manifest" "bootstrap_issuer" {
  count = var.issuer_name != "" ? 1 : 0

  yaml_body         = <<-YAML
    apiVersion: cert-manager.io/v1
    kind: ClusterIssuer
    metadata:
      name: ${var.issuer_name}-bootstrap
    spec:
      selfSigned: {}
  YAML
  server_side_apply = true
  field_manager     = "tofu-e2e-harbor-bridge-install"
  wait_for {
    condition {
      type   = "Ready"
      status = "True"
    }
  }
}

resource "kubectl_manifest" "root_ca" {
  count = var.issuer_name != "" ? 1 : 0

  yaml_body         = <<-YAML
    apiVersion: cert-manager.io/v1
    kind: Certificate
    metadata:
      name: ${var.issuer_name}-root
      namespace: ${var.cert_manager_namespace}
    spec:
      isCA: true
      commonName: ${var.issuer_name}-root
      secretName: ${var.issuer_name}-root
      duration: 2160h
      privateKey:
        algorithm: ECDSA
        size: 256
      issuerRef:
        name: ${var.issuer_name}-bootstrap
        kind: ClusterIssuer
        group: cert-manager.io
  YAML
  server_side_apply = true
  field_manager     = "tofu-e2e-harbor-bridge-install"
  wait_for {
    condition {
      type   = "Ready"
      status = "True"
    }
  }
  depends_on = [kubectl_manifest.bootstrap_issuer]
}

resource "kubectl_manifest" "cluster_issuer" {
  count = var.issuer_name != "" ? 1 : 0

  yaml_body         = <<-YAML
    apiVersion: cert-manager.io/v1
    kind: ClusterIssuer
    metadata:
      name: ${var.issuer_name}
    spec:
      ca:
        secretName: ${var.issuer_name}-root
  YAML
  server_side_apply = true
  field_manager     = "tofu-e2e-harbor-bridge-install"
  wait_for {
    condition {
      type   = "Ready"
      status = "True"
    }
  }
  depends_on = [kubectl_manifest.root_ca]
}

# docs/platforms.md (GKE) shows the same quota. One pod per node; the
# limit only has to exceed the node count.
resource "kubernetes_resource_quota_v1" "critical_pods" {
  count = var.critical_pods_quota ? 1 : 0

  metadata {
    name      = "harbor-bridge-critical-pods"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    hard = {
      pods = "1000"
    }
    scope_selector {
      match_expression {
        scope_name = "PriorityClass"
        operator   = "In"
        values     = ["system-node-critical"]
      }
    }
  }
}

# CRDs are installed automatically by helm from the chart's crds/
# directory on first install. No separate kubectl_manifest needed.

resource "helm_release" "bridge" {
  name      = var.release_name
  namespace = kubernetes_namespace_v1.this.metadata[0].name
  chart     = abspath("${path.module}/${var.chart_path}")
  timeout   = 600
  # null_resource.bridge_rollout waits instead. A helm wait that times out
  # fails this resource, and nothing runs after it to collect diagnostics:
  # compat run 42784591046 lost a bridge_upgrade timeout that way.
  wait   = false
  atomic = false
  # The chart's pre-delete hook has every node remove the plugin
  # (ADR-0039). The node_uninstall stage runs it on purpose with the helm
  # CLI; the cleanup after a failed stage must not wait for it.
  disable_webhooks = true

  values = [yamlencode(merge({
    clusterName = var.cluster_name
    harbor = {
      url = var.harbor_url
      # The test Harbor is reached over the pod network (harbor-core
      # Service, plain http); production uses https.
      allowInsecureHTTP = startswith(var.harbor_url, "http://")
      adminCredsSecret = {
        name = kubernetes_secret_v1.admin.metadata[0].name
      }
    }
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
      var.provider_name != "" ? { providerName = var.provider_name } : {},
    )
    bridge = merge({
      oidcIssuer  = var.oidc_issuer
      oidcJWKSURL = var.oidc_jwks_url
      replicas    = var.bridge_replicas
      logLevel    = "debug"
      image       = var.bridge_image
      resources   = var.bridge_resources
      # Without issuer_name the release CA signs the client certificate too.
      mTLS = merge(
        { enabled = var.mtls },
        var.issuer_name != "" ? { clientIssuerRef = { name = var.issuer_name, kind = "ClusterIssuer" } } : {},
      )
    }, length(var.harbor_access_selector) > 0 ? { harborAccessSelector = var.harbor_access_selector } : {})
    tls = merge(
      { enabled = true },
      var.issuer_name != "" ? { issuerRef = { name = var.issuer_name, kind = "ClusterIssuer" } } : {},
    )
  }, var.node_port > 0 ? { service = { nodePort = var.node_port } } : {}))]

  depends_on = [
    kubectl_manifest.cluster_issuer,
    kubernetes_resource_quota_v1.critical_pods,
  ]
}

# The release's only wait (helm_release.bridge sets wait = false). First
# the plugin DaemonSet: each new pod's installer restarts kubelet when the
# provider config changed, so this can take minutes. Then the bridge
# Deployment, stricter than helm's wait: helm counts a Deployment as ready
# once replicas - maxUnavailable pods are (1 of 2 with the chart's
# maxUnavailable: 1), and the Service then routes everything to the one
# ready pod. A replica that never becomes Ready, such as a follower whose
# data plane waits for the leadership it never gets (audit H1), passed the
# install and every pull. Wait for the whole rollout and require every
# replica updated, Ready and available, after each install or upgrade
# that changed the release. On failure the rollout status, both
# components' pods and logs, the events and each kind node's kubelet
# journal land in .diag/bridge-rollout/ (this provisioner's own output is
# suppressed: its environment is sensitive).
resource "null_resource" "bridge_rollout" {
  depends_on = [helm_release.bridge]

  lifecycle {
    replace_triggered_by = [helm_release.bridge]
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      source "$KUBECONFIG_LIB" && harness_kubeconfig "$d" || exit 1
      fail() {
        rm -rf "$DIAG"; mkdir -p "$DIAG"
        echo "$1" > "$DIAG/FAILED"
        cp "$d/rollout.txt" "$DIAG/rollout.txt" 2>/dev/null
        k -n "$NS" get pods -o wide > "$DIAG/pods.txt" 2>&1
        k -n "$NS" get events --sort-by=.lastTimestamp > "$DIAG/events.txt" 2>&1
        k -n "$NS" get deployment "$DEPLOYMENT" -o yaml > "$DIAG/deployment.yaml" 2>&1
        k -n "$NS" describe pods -l app.kubernetes.io/component=bridge > "$DIAG/pods.describe.txt" 2>&1
        k -n "$NS" logs -l app.kubernetes.io/component=bridge --all-containers --tail=1000 --prefix > "$DIAG/bridge.log" 2>&1
        k -n "$NS" get daemonset "$DAEMONSET" -o yaml > "$DIAG/daemonset.yaml" 2>&1
        k -n "$NS" describe pods -l app.kubernetes.io/component=plugin > "$DIAG/plugin-pods.describe.txt" 2>&1
        k -n "$NS" logs -l app.kubernetes.io/component=plugin -c install --tail=1000 --prefix > "$DIAG/install.log" 2>&1
        # kind node names are their container names. Elsewhere (GKE)
        # docker exec fails and the file holds that error.
        for n in $(k get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
          docker exec "$n" journalctl -u kubelet --no-pager --since -20min > "$DIAG/kubelet-$n.log" 2>&1
        done
        echo "FAILED: $1 (diagnostics: $DIAG)" >&2
        exit 1
      }
      k -n "$NS" rollout status "daemonset/$DAEMONSET" --timeout=600s > "$d/rollout.txt" 2>&1 \
        || fail "the plugin DaemonSet did not finish its rollout within 600s"
      k -n "$NS" rollout status "deployment/$DEPLOYMENT" --timeout=300s > "$d/rollout.txt" 2>&1 \
        || fail "the bridge Deployment did not finish its rollout within 300s"
      got="$(k -n "$NS" get deployment "$DEPLOYMENT" -o jsonpath='{.spec.replicas} {.status.updatedReplicas} {.status.readyReplicas} {.status.availableReplicas}')" \
        || fail "could not read the bridge Deployment"
      read -r spec updated ready available <<< "$got"
      for n in "$spec" "$${updated:-0}" "$${ready:-0}" "$${available:-0}"; do
        [ "$n" = "$REPLICAS" ] || fail "bridge Deployment: $${spec:-?} replicas, $${updated:-0} updated, $${ready:-0} Ready, $${available:-0} available; want $REPLICAS of each"
      done
    BASH
    environment = {
      K8S_HOST       = var.kubeconfig.host
      K8S_CA         = var.kubeconfig.cluster_ca_certificate
      K8S_CERT       = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
      K8S_KEY        = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
      K8S_TOKEN      = var.kubeconfig.token == null ? "" : var.kubeconfig.token
      KUBECONFIG_LIB = abspath("${path.module}/../../scripts/kubeconfig.sh")
      NS             = helm_release.bridge.namespace
      DEPLOYMENT     = helm_release.bridge.name
      DAEMONSET      = "${helm_release.bridge.name}-plugin"
      REPLICAS       = tostring(var.bridge_replicas)
      DIAG           = abspath("${path.cwd}/.diag/bridge-rollout")
    }
  }
}

# Harness-only, not part of the chart: a headless Service over the bridge
# pods, Ready or not, so that DNS answers with one address per replica. The
# bridge_replicas stage asks each replica for credentials at its own
# address; the chart's Service would pick one, and hide a replica that
# does not serve.
resource "kubernetes_service_v1" "bridge_pods" {
  metadata {
    name      = "${helm_release.bridge.name}-pods"
    namespace = helm_release.bridge.namespace
  }
  spec {
    cluster_ip                  = "None"
    publish_not_ready_addresses = true
    # The chart's bridge selector labels (harbor-bridge.bridge.selectorLabels).
    selector = {
      "app.kubernetes.io/name"      = "harbor-workload-identity-bridge"
      "app.kubernetes.io/instance"  = helm_release.bridge.name
      "app.kubernetes.io/component" = "bridge"
    }
    port {
      name        = "https"
      port        = 8443
      target_port = "https"
    }
  }
}

output "namespace" {
  value = kubernetes_namespace_v1.this.metadata[0].name
}

output "release" {
  value       = helm_release.bridge.name
  description = "Helm release name of the bridge (and of its plugin DaemonSet, <release>-plugin)."
}

output "bridge_replicas" {
  value       = var.bridge_replicas
  description = "Bridge replicas the install waited for."
}

# The Secret holding the plugin's client certificate (tls.crt, tls.key and
# the issuing CA as ca.crt) when mtls is on: the chart's
# harbor-bridge.mTLSClientSecretName, in the release namespace (this
# module leaves plugin.namespace empty).
output "mtls_client_secret" {
  value = var.mtls ? "${helm_release.bridge.name}-plugin-mtls-client" : ""
}

output "bridge_pods_host" {
  value       = "${kubernetes_service_v1.bridge_pods.metadata[0].name}.${kubernetes_service_v1.bridge_pods.metadata[0].namespace}.svc"
  description = "Headless Service name that resolves to every bridge pod's address, Ready or not (for checks that must reach each replica)."
}

# The credential endpoint through the bridge's Service, for in-cluster
# checks that call the bridge directly (the plugin uses the NodePort). The
# Service carries the release name (the chart's fullname) and the chart's
# default service.port 8443, which this module does not override; the
# serving certificate names this host.
output "credentials_url" {
  value = "https://${helm_release.bridge.name}.${helm_release.bridge.namespace}.svc:8443/v1/credentials"
}

# Teardown guard. HarborAccess objects carry the bridge's finalizer, which
# only a running bridge releases. tofu test destroys states in reverse
# order of the LAST run that touched each, so after a failure (or with any
# stage order that ends on an install/upgrade run) the bridge used to be
# uninstalled while HarborAccess objects still existed — their deletion,
# and that of their namespaces, then hung until the cleanup timed out.
#
# This resource depends on the release, so tofu destroys it FIRST: its
# destroy-time provisioner deletes every HarborAccess while the bridge is
# still up (the finalizers revoke the robots, as they would for a user),
# and only then does helm uninstall the chart. If the bridge cannot
# release them in time (e.g. Harbor already gone), the finalizers are
# dropped so the throwaway cluster can be torn down — the test run is
# ending anyway, and a real deletion path is asserted by the scenario's
# teardown stage while everything is healthy.
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
    token_command  = var.token_command
    kubeconfig_lib = abspath("${path.module}/../../scripts/kubeconfig.sh")
    k8s_host       = var.kubeconfig.host
    k8s_ca         = var.kubeconfig.cluster_ca_certificate
    k8s_cert       = var.kubeconfig.client_certificate == null ? "" : var.kubeconfig.client_certificate
    k8s_key        = var.kubeconfig.client_key == null ? "" : var.kubeconfig.client_key
    k8s_token      = var.kubeconfig.token == null ? "" : var.kubeconfig.token
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]
    command     = <<-BASH
      set -uo pipefail
      d="$(mktemp -d)"
      trap 'rm -rf "$d"' EXIT
      if [ -n "$TOKEN_COMMAND" ]; then
        # A command substitution: the fresh token never reaches an argv.
        K8S_TOKEN="$(bash -c "$TOKEN_COMMAND")"
      fi
      if ! { source "$KUBECONFIG_LIB" && harness_kubeconfig "$d"; }; then
        echo "could not write the kubeconfig; nothing released" >&2
        exit 0
      fi
      if ! k get crd harboraccesses.harbor.aetherize.io >/dev/null 2>&1; then
        echo "no HarborAccess CRD reachable (or no API access); nothing to release" >&2
        exit 0
      fi
      if k delete harboraccesses.harbor.aetherize.io --all -A --wait=true --timeout=180s; then
        exit 0
      fi
      echo "HarborAccess deletion did not finish; dropping finalizers so teardown can proceed" >&2
      for o in $(k get harboraccesses.harbor.aetherize.io -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{end}'); do
        k -n "$${o%/*}" patch harboraccess "$${o#*/}" --type=merge -p '{"metadata":{"finalizers":null}}' || true
      done
      exit 0
    BASH
    environment = {
      K8S_HOST       = self.triggers.k8s_host
      K8S_CA         = self.triggers.k8s_ca
      K8S_CERT       = self.triggers.k8s_cert
      K8S_KEY        = self.triggers.k8s_key
      K8S_TOKEN      = self.triggers.k8s_token
      TOKEN_COMMAND  = self.triggers.token_command
      KUBECONFIG_LIB = self.triggers.kubeconfig_lib
    }
  }
}
