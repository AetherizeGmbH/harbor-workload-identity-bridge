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
    client_certificate     = string
    client_key             = string
    cluster_ca_certificate = string
  })
  sensitive = true
}

variable "dns_rewrite_targets" {
  type        = map(string)
  default     = {}
  description = <<-EOT
    Map of suffix → CNAME target. For each entry, CoreDNS gets a
    `template IN ANY <key>` block that CNAMEs the matched query to
    the target. Use to make in-cluster pods resolve nip.io-style
    hostnames to in-cluster services (ingress controllers, etc.).
  EOT
}

variable "dns_hosts_entries" {
  type        = map(string)
  default     = {}
  description = <<-EOT
    Map of hostname → IP. Materialised into CoreDNS's `hosts` plugin
    (an /etc/hosts-style A-record table) so every in-cluster pod can
    resolve the hostname. Use for synthetic names that have to point
    at something CNAME can't reach — e.g. `harbor.e2e → <kind_node_ip>`,
    where the target is a kind-node docker IP (no in-cluster DNS).
  EOT
}

provider "kubernetes" {
  host                   = var.kubeconfig.host
  client_certificate     = var.kubeconfig.client_certificate
  client_key             = var.kubeconfig.client_key
  cluster_ca_certificate = var.kubeconfig.cluster_ca_certificate
}

resource "kubernetes_config_map_v1_data" "coredns" {
  metadata {
    namespace = "kube-system"
    name      = "coredns"
  }
  force = true
  data = {
    Corefile = <<-EOT
      .:53 {
        errors
        health {
           lameduck 5s
        }
        ready
        kubernetes cluster.local in-addr.arpa ip6.arpa {
           pods insecure
           fallthrough in-addr.arpa ip6.arpa
           ttl 30
        }
        %{for k, v in var.dns_rewrite_targets~}
        template IN ANY ${k} {
          answer "{{ .Name }} 60 IN CNAME ${v}"
        }
        %{endfor~}
        %{if length(var.dns_hosts_entries) > 0~}
        hosts {
          %{for hostname, ip in var.dns_hosts_entries~}
          ${ip} ${hostname}
          %{endfor~}
          fallthrough
        }
        %{endif~}
        prometheus :9153
        forward . /etc/resolv.conf {
           max_concurrent 1000
        }
        cache 30 {
           disable success cluster.local
           disable denial cluster.local
        }
        loop
        reload
        loadbalance
      }
    EOT
  }
}

# CoreDNS doesn't hot-reload at-rest configmap changes reliably; the
# `reload` plugin watches with TTL. Just nudge the deployment so the
# new Corefile is in effect when other modules depend on us.
resource "null_resource" "coredns_reload" {
  triggers = {
    corefile_sha = sha256(kubernetes_config_map_v1_data.coredns.data.Corefile)
  }
  provisioner "local-exec" {
    command = "kubectl --kubeconfig=$KUBECONFIG -n kube-system rollout restart deployment/coredns && kubectl --kubeconfig=$KUBECONFIG -n kube-system rollout status deployment/coredns --timeout=60s"
    environment = {
      KUBECONFIG = pathexpand("~/.kube/config")
    }
  }
  depends_on = [kubernetes_config_map_v1_data.coredns]
}
