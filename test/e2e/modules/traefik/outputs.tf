output "service_name" {
  value       = "traefik.${helm_release.traefik.namespace}.svc.cluster.local"
  description = "In-cluster CNAME target for nip.io hostnames Traefik serves."
}

output "namespace" {
  value = helm_release.traefik.namespace
}
