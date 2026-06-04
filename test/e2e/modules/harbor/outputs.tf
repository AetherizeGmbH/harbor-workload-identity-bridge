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
  description = "External URL exposed via NodePort. Containerd and crane use this for image push/pull."
}

output "external_host" {
  value       = "${var.external_hostname}:${var.https_node_port}"
  description = "host:port the test image refs use."
}
