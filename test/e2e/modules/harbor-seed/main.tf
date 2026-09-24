# Seeds Harbor for the e2e scenarios: creates every project and pushes the
# test image into each, then WAITS for the seeding Job to complete.
#
# Two properties matter and both used to be missing:
#   - It waits. The old seed_image run applied the Job and moved on, so
#     later stages could race a half-finished seed (projects late in the
#     list had no image yet: "beta-1/app:v1: not found").
#   - It owns its own OpenTofu state. tofu test keys state by module
#     source; seed_image and harbor_access both used ./modules/k8s-yaml
#     directly, so applying harbor_access REPLACED the seed objects and
#     deleted the e2e-seed namespace — with the seed Job possibly still
#     running. Wrapping k8s-yaml in this module gives the seed a separate
#     state, so it stays put (its harbor-admin Secret is reused by the
#     robot checks later in the run).
terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
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

variable "admin_password" {
  type      = string
  sensitive = true
}

variable "namespace" {
  type    = string
  default = "e2e-seed"
}

variable "registry_host" {
  type        = string
  default     = "harbor.e2e:30843"
  description = "External Harbor host[:port] the image refs use (matches Harbor's externalURL, so crane accepts the auth realm). Without a port, 443."
}

variable "core_url" {
  type        = string
  default     = "http://harbor-core.harbor.svc.cluster.local"
  description = "In-cluster Harbor API base URL used to create the projects."
}

variable "projects" {
  type        = list(string)
  description = "Projects to create; each receives <project>/app:v1."
}

variable "seed_image" {
  type        = string
  default     = "e2e-seed:e2e"
  description = "Image with crane + curl + openssl (test/e2e/seed/Dockerfile), kind-loaded."
}

locals {
  projects = join(" ", var.projects)
  # Taken from the created resources (not var.namespace) so the manifests
  # depend on them: depends_on is not allowed on k8s-yaml, which
  # configures its own provider.
  ns           = kubernetes_namespace_v1.seed.metadata[0].name
  admin_secret = kubernetes_secret_v1.admin.metadata[0].name
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

resource "kubernetes_namespace_v1" "seed" {
  metadata { name = var.namespace }
}

# A native resource, not a k8s-yaml manifest: the password would make the
# whole manifest list sensitive, and sensitive values cannot key for_each.
resource "kubernetes_secret_v1" "admin" {
  metadata {
    name      = "harbor-admin"
    namespace = kubernetes_namespace_v1.seed.metadata[0].name
  }
  data = {
    username = "admin"
    password = var.admin_password
  }
  type = "Opaque"
}

module "manifests" {
  source     = "../k8s-yaml"
  kubeconfig = var.kubeconfig
  manifests = [
    {
      yaml = <<-YAML
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: project-bootstrap
          namespace: ${local.ns}
        data:
          run.sh: |
            #!/bin/sh
            set -eu
            host="${var.registry_host}"
            case "$host" in *:*) addr="$host" ;; *) addr="$host:443" ;; esac
            # Grab Harbor's self-signed leaf cert off the wire and add it
            # to the system trust store (reading the live endpoint is
            # robust against chart secret-naming changes).
            openssl s_client -connect "$addr" -servername "$${host%%:*}" </dev/null 2>/dev/null \
              | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
              > /usr/local/share/ca-certificates/harbor-e2e.crt
            test -s /usr/local/share/ca-certificates/harbor-e2e.crt
            update-ca-certificates 2>/dev/null

            user="$(cat /admin/username)"
            pass="$(cat /admin/password)"
            for proj in ${local.projects}; do
              code="$(curl -s -o /dev/null -w '%%{http_code}' -u "$user:$pass" -X POST \
                -H "Content-Type: application/json" \
                "${var.core_url}/api/v2.0/projects" \
                -d '{"project_name":"'"$proj"'","metadata":{"public":"false"}}')"
              # 201 created, 409 exists (re-run) — anything else is fatal.
              case "$code" in 201|409) ;; *) echo "create project $proj: HTTP $code"; exit 1 ;; esac
            done

            crane auth login "$host" -u "$user" -p "$pass"
            # One docker.io pull total; every project gets an intra-Harbor copy.
            crane copy alpine:3.20 "$host/your-project/alpine:test3"
            for proj in ${local.projects}; do
              crane copy "$host/your-project/alpine:test3" "$host/$proj/app:v1"
            done
            # Verify, so a partial seed fails HERE and not as a confusing
            # "not found" pull failure several stages later.
            for proj in ${local.projects}; do
              crane digest "$host/$proj/app:v1" >/dev/null
            done
            echo "seeded: ${local.projects}"
      YAML
    },
    {
      yaml = <<-YAML
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: seed-image
          namespace: ${local.ns}
        spec:
          backoffLimit: 2
          template:
            spec:
              restartPolicy: Never
              containers:
                - name: seed
                  image: ${var.seed_image}
                  # Exists only on the kind nodes (kind load).
                  imagePullPolicy: IfNotPresent
                  command: [sh, /scripts/run.sh]
                  volumeMounts:
                    - { name: scripts, mountPath: /scripts }
                    - { name: admin, mountPath: /admin, readOnly: true }
              volumes:
                - name: scripts
                  configMap:
                    name: project-bootstrap
                    # 493 decimal = 0o755 (yamldecode would read 0755 as 755).
                    defaultMode: 493
                - name: admin
                  secret:
                    secretName: ${local.admin_secret}
      YAML
      wait = {
        conditions = [{ type = "Complete", status = "True" }]
      }
    },
  ]
}

output "namespace" {
  value       = var.namespace
  description = "Namespace holding the harbor-admin Secret (reused by the robot-check Jobs)."
}

output "admin_secret_name" {
  value = kubernetes_secret_v1.admin.metadata[0].name
}
