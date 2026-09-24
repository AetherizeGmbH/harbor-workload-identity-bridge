provider "helm" {
  kubernetes = {
    host                   = local.cluster_conn.host
    client_certificate     = local.cluster_conn.client_certificate
    client_key             = local.cluster_conn.client_key
    cluster_ca_certificate = local.cluster_conn.cluster_ca_certificate
  }
}
