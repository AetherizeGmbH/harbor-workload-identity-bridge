output "name" {
  value       = kind_cluster.this.name
  description = "kind cluster name"
}

output "kubeconfig" {
  value = {
    host                   = kind_cluster.this.endpoint
    client_certificate     = kind_cluster.this.client_certificate
    client_key             = kind_cluster.this.client_key
    cluster_ca_certificate = kind_cluster.this.cluster_ca_certificate
  }
  description = "Connection info that other modules use to talk to the cluster."
  sensitive   = true
}

output "node_names" {
  value       = local.node_names
  description = "Docker container names of every kind node (control-plane + workers). Other modules `docker exec` into these to write hosts.toml, /etc/hosts, ca.crt, etc."
}

output "ports" {
  value = {
    host_http       = var.http_port
    host_https      = var.https_port
    node_port_http  = local.http_node_port_internal
    node_port_https = local.https_node_port_internal
  }
  description = "Port mapping summary. host_* are the laptop-facing ports kind binds; node_port_* are the kind-internal NodePorts they map to."
}
