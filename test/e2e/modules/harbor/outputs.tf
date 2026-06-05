output "namespace" {
  value = helm_release.harbor.namespace
}

output "admin_password" {
  value     = random_password.admin.result
  sensitive = true
}

output "internal_api_url" {
  value       = "http://harbor-core.${helm_release.harbor.namespace}.svc.cluster.local"
  description = "In-cluster URL the bridge reaches the Harbor REST API on (HTTP, no TLS quirks)."
}

output "external_url" {
  value       = "https://${var.external_hostname}:${var.https_node_port}"
  description = "External URL Harbor advertises in API responses and registry www-authenticate realms."
}

output "external_host" {
  value       = "${var.external_hostname}:${var.https_node_port}"
  description = "host:port the test image refs use."
}

data "kubernetes_nodes" "this" {}

output "kind_node_ip" {
  description = <<-EOT
    InternalIP of the first kind node. Pods inside the cluster reach
    kind nodes by their docker-network IP (typically 172.18.0.x), so a
    pod with a hostAlias mapping `harbor.e2e → <kind_node_ip>` reaches
    the Harbor NodePort listener via that node's host netns.
  EOT
  value = one([
    for a in data.kubernetes_nodes.this.nodes[0].status[0].addresses :
    a.address if a.type == "InternalIP"
  ])
}

# NB: previous revisions exposed Harbor's TLS leaf via a
# data.kubernetes_secret lookup at `harbor-ingress`. That Secret name
# varies between chart versions (different release-name prefixing,
# different exposure modes), so the lookup is fragile. The seed Job
# instead grabs the cert off the wire with `openssl s_client` at
# runtime — works for any chart version that serves TLS at all.
