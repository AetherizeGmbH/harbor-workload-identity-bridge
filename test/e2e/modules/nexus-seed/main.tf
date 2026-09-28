# Seeds Nexus for the NexusAccess scenario and WAITS for the seeding Job:
#
#   1. realms: the local realm and DockerToken (docker bearer tokens,
#      ADR-0033 Context 1);
#   2. anonymous access off, so every pull needs credentials;
#   3. one hosted docker repository per entry of var.repositories, each
#      with its own HTTP connector (Nexus 3.76 routes docker requests by
#      port);
#   4. the bridge's own Nexus user: a random name, never "admin", holding
#      only nx-users-all, nx-roles-all and nx-privileges-read, the least
#      privilege ADR-0033 decision j names. The bridge runs with it, so the
#      e2e proves that nothing the bridge does needs more;
#   5. <repository>/app:v1 in every repository, copied with crane from
#      var.source_image for the node's platform, and read back.
#
# Like harbor-seed it owns its state (it wraps k8s-yaml), so the scenario
# runs cannot replace the seed objects, and the check Jobs reuse its
# nexus-admin Secret.
terraform {
  required_version = ">= 1.6"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
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
  type        = string
  sensitive   = true
  description = "Password of Nexus's admin user (the nexus module's admin_password)."
}

variable "namespace" {
  type    = string
  default = "nexus-seed"
}

variable "nexus_url" {
  type        = string
  description = "Nexus base URL inside the cluster, without /service/rest."
}

variable "repositories" {
  type = map(object({
    connector_port = number
    registry_host  = string
  }))
  description = "Hosted docker repositories to create: connector port and the host:port image references use (the nexus module's repositories output)."
}

variable "source_image" {
  type        = string
  default     = "alpine:3.20"
  description = "Image copied into every repository as app:v1 (one pull from its registry)."
}

variable "seed_image" {
  type        = string
  default     = "e2e-seed:e2e"
  description = "Image with crane + curl + jq (test/e2e/seed/Dockerfile), kind-loaded."
}

locals {
  bridge_admin_role = "hwib-e2e-bridge-admin"
  # One word per repository for the script's loops:
  # <name>:<connector port>:<registry host:port>.
  repositories = join(" ", [for name, r in var.repositories : "${name}:${r.connector_port}:${r.registry_host}"])
  ns           = kubernetes_namespace_v1.seed.metadata[0].name
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  token                  = var.kubeconfig.token
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

resource "random_string" "bridge_admin" {
  length  = 20
  upper   = false
  special = false
}

resource "random_password" "bridge_admin" {
  length  = 32
  special = false
}

resource "kubernetes_namespace_v1" "seed" {
  metadata { name = var.namespace }
}

# Native resources, not k8s-yaml manifests: a password would make the
# whole manifest list sensitive, and sensitive values cannot key for_each.
resource "kubernetes_secret_v1" "admin" {
  metadata {
    name      = "nexus-admin"
    namespace = local.ns
  }
  data = {
    username = "admin"
    password = var.admin_password
  }
  type = "Opaque"
}

resource "kubernetes_secret_v1" "bridge_admin" {
  metadata {
    name      = "nexus-bridge-admin"
    namespace = local.ns
  }
  data = {
    username = "hwib-e2e-${random_string.bridge_admin.result}"
    password = random_password.bridge_admin.result
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
          name: nexus-bootstrap
          namespace: ${local.ns}
        data:
          run.sh: |
            #!/bin/sh
            set -eu
            api='${var.nexus_url}/service/rest'
            user="$(cat /admin/username)"; pass="$(cat /admin/password)"
            buser="$(cat /bridge-admin/username)"; bpass="$(cat /bridge-admin/password)"
            role='${local.bridge_admin_role}'

            # nx METHOD PATH [JSON]: HTTP status on stdout, body in /tmp/nx.json.
            nx() {
              if [ $# -ge 3 ]; then
                c=$(curl -sS -o /tmp/nx.json -w '%%{http_code}' -u "$user:$pass" -X "$1" \
                  -H 'Content-Type: application/json' --data "$3" "$api$2") || c=000
              else
                c=$(curl -sS -o /tmp/nx.json -w '%%{http_code}' -u "$user:$pass" -X "$1" "$api$2") || c=000
              fi
              echo "$c"
            }
            fail() { echo "FAIL: $*" >&2; exit 1; }

            # 1. Writable. The Deployment's startup probe gated the rollout on
            #    it; this guards a seed that runs right after a restart.
            i=0
            until curl -fsS -o /dev/null "$api/v1/status/writable"; do
              i=$((i + 1)); [ "$i" -lt 60 ] || fail "Nexus is not writable"
              sleep 5
            done

            # 2. Realms: local users, and DockerToken for docker bearer tokens.
            c=$(nx PUT /v1/security/realms/active '["NexusAuthenticatingRealm","DockerToken"]')
            case "$c" in 200|204) ;; 401) fail "the admin password does not work (NEXUS_SECURITY_INITIAL_PASSWORD)" ;; *) fail "set realms: HTTP $c: $(cat /tmp/nx.json)" ;; esac
            c=$(nx GET /v1/security/realms/active); [ "$c" = 200 ] || fail "read realms: HTTP $c"
            jq -e 'index("NexusAuthenticatingRealm") != null and index("DockerToken") != null' /tmp/nx.json >/dev/null \
              || fail "realms not active: $(cat /tmp/nx.json)"

            # 3. Anonymous access off.
            c=$(nx GET /v1/security/anonymous); [ "$c" = 200 ] || fail "read anonymous access: HTTP $c"
            c=$(nx PUT /v1/security/anonymous "$(jq -c '.enabled = false' /tmp/nx.json)")
            case "$c" in 200|204) ;; *) fail "disable anonymous access: HTTP $c: $(cat /tmp/nx.json)" ;; esac
            c=$(nx GET /v1/security/anonymous); [ "$c" = 200 ] || fail "read anonymous access: HTTP $c"
            jq -e '.enabled == false' /tmp/nx.json >/dev/null || fail "anonymous access still on"

            # 4. Hosted docker repositories, one HTTP connector each.
            c=$(nx GET /v1/repositories); [ "$c" = 200 ] || fail "list repositories: HTTP $c"
            cp /tmp/nx.json /tmp/repositories.json
            for r in ${local.repositories}; do
              name=$${r%%:*}; rest=$${r#*:}; port=$${rest%%:*}
              if jq -e --arg n "$name" 'any(.[]; .name == $n)' /tmp/repositories.json >/dev/null; then continue; fi
              body=$(jq -cn --arg n "$name" --argjson p "$port" '{
                name: $n, online: true,
                storage: {blobStoreName: "default", strictContentTypeValidation: true, writePolicy: "allow"},
                docker: {v1Enabled: false, forceBasicAuth: false, httpPort: $p}}')
              c=$(nx POST /v1/repositories/docker/hosted "$body")
              [ "$c" = 201 ] || fail "create repository $name: HTTP $c: $(cat /tmp/nx.json)"
            done

            # 5. The bridge's user, with exactly the privileges ADR-0033
            #    decision j names.
            c=$(nx GET "/v1/security/roles/$role?source=default")
            case "$c" in
              200) ;;
              404)
                body=$(jq -cn --arg r "$role" '{id: $r, name: $r,
                  description: "e2e: least-privilege Nexus role of the bridge (ADR-0033 decision j)",
                  privileges: ["nx-users-all", "nx-roles-all", "nx-privileges-read"], roles: []}')
                c=$(nx POST /v1/security/roles "$body")
                [ "$c" = 200 ] || fail "create role $role: HTTP $c: $(cat /tmp/nx.json)"
                ;;
              *) fail "read role $role: HTTP $c" ;;
            esac
            c=$(nx GET "/v1/security/users?source=default&userId=$buser"); [ "$c" = 200 ] || fail "list users: HTTP $c"
            if ! jq -e --arg u "$buser" 'any(.[]; .userId == $u)' /tmp/nx.json >/dev/null; then
              body=$(jq -cn --arg u "$buser" --arg p "$bpass" --arg r "$role" '{userId: $u,
                firstName: "hwib-e2e", lastName: "bridge", emailAddress: "bridge-admin@hwib-e2e.invalid",
                password: $p, status: "active", roles: [$r]}')
              c=$(nx POST /v1/security/users "$body")
              [ "$c" = 200 ] || fail "create the bridge's user: HTTP $c"
            fi
            # It works for what the bridge does, and it is not an administrator.
            c=$(curl -sS -o /dev/null -w '%%{http_code}' -u "$buser:$bpass" "$api/v1/security/users?source=default&userId=bridge-") || c=000
            [ "$c" = 200 ] || fail "the bridge's user cannot list users: HTTP $c"
            c=$(curl -sS -o /dev/null -w '%%{http_code}' -u "$buser:$bpass" "$api/v1/security/realms/active") || c=000
            [ "$c" = 403 ] || fail "the bridge's user reads the realm configuration (HTTP $c): it has more than decision j's privileges"

            # 6. Every connector answers, and refuses anonymous pulls with a
            #    bearer challenge (DockerToken active, anonymous off).
            for r in ${local.repositories}; do
              host=$${r#*:*:}
              i=0
              until [ "$(curl -s -o /dev/null -w '%%{http_code}' "http://$host/v2/" || true)" = 401 ]; do
                i=$((i + 1)); [ "$i" -lt 40 ] || fail "connector http://$host/v2/ does not answer 401"
                sleep 3
              done
              curl -sS -o /dev/null -D /tmp/headers "http://$host/v2/" || true
              grep -qi '^www-authenticate: bearer realm=' /tmp/headers || fail "http://$host/v2/ does not challenge for a bearer token: $(cat /tmp/headers)"
            done

            # 7. <repository>/app:v1 everywhere: one pull from the source
            #    registry for the node's platform, then Nexus to Nexus.
            case "$(uname -m)" in
              x86_64) platform=linux/amd64 ;;
              aarch64) platform=linux/arm64 ;;
              *) fail "unexpected machine $(uname -m)" ;;
            esac
            first=""
            for r in ${local.repositories}; do
              host=$${r#*:*:}
              crane auth login "$host" -u "$user" -p "$pass"
              if [ -z "$first" ]; then
                crane copy --insecure --platform "$platform" '${var.source_image}' "$host/app:v1"
                first=$host
              else
                crane copy --insecure "$first/app:v1" "$host/app:v1"
              fi
            done
            for r in ${local.repositories}; do
              host=$${r#*:*:}
              crane digest --insecure "$host/app:v1" >/dev/null || fail "$host/app:v1 not readable after the copy"
              # Anonymous access is really off for the image.
              c=$(curl -s -o /dev/null -w '%%{http_code}' -H "Accept: application/vnd.docker.distribution.manifest.v2+json" "http://$host/v2/app/manifests/v1" || true)
              [ "$c" = 401 ] || fail "anonymous manifest read at $host: HTTP $c, want 401"
            done
            echo "seeded Nexus: ${local.repositories}"
      YAML
    },
    {
      yaml = <<-YAML
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: seed-nexus
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
                    - { name: bridge-admin, mountPath: /bridge-admin, readOnly: true }
              volumes:
                - name: scripts
                  configMap:
                    name: nexus-bootstrap
                    # 493 decimal = 0o755 (yamldecode would read 0755 as 755).
                    defaultMode: 493
                - name: admin
                  secret:
                    secretName: ${kubernetes_secret_v1.admin.metadata[0].name}
                - name: bridge-admin
                  secret:
                    secretName: ${kubernetes_secret_v1.bridge_admin.metadata[0].name}
      YAML
      wait = {
        conditions = [{ type = "Complete", status = "True" }]
      }
    },
  ]
}

output "namespace" {
  value       = local.ns
  description = "Namespace holding the nexus-admin Secret (reused by the check Jobs)."
}

output "admin_secret_name" {
  value = kubernetes_secret_v1.admin.metadata[0].name
}

output "bridge_admin_username" {
  value       = kubernetes_secret_v1.bridge_admin.data.username
  sensitive   = true
  description = "The bridge's Nexus user (ADR-0033 decision j recommends keeping even the name out of sight)."
}

output "bridge_admin_password" {
  value     = random_password.bridge_admin.result
  sensitive = true
}

output "bridge_admin_role" {
  value = local.bridge_admin_role
}
