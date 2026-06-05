output "image_tags" {
  description = "Map of friendly name to the built image's tag (the same tag you passed in)."
  value       = { for k, v in docker_image.build : k => v.name }
}

output "image_tags_list" {
  description = "Flat list of all built image tags. Pass directly to kind-cluster's images_to_load."
  value       = [for v in docker_image.build : v.name]
}

output "image_ids" {
  description = "Map of friendly name to the built image's content-addressed sha256 ID. The provider populates this from `docker image inspect` after the build completes."
  value       = { for k, v in docker_image.build : k => v.image_id }
}

output "build_id" {
  description = <<-EOT
    Aggregate sha256 over every built image's content ID. Rotates
    whenever any image rebuilds; stable across no-op applies.
    Reference this in downstream stages to gate work behind a
    successful build.
  EOT
  value       = sha256(jsonencode({ for k, v in docker_image.build : k => v.image_id }))
}
