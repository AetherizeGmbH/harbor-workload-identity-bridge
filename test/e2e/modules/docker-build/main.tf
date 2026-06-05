resource "docker_image" "build" {
  for_each = var.images

  name = each.value.tag

  # keep_locally = true so the docker_image's destroy is a no-op. Keeps
  # iter-test cycles fast (no remove + rebuild), and CI runners are
  # ephemeral anyway so there's nothing to leak.
  keep_locally = true

  build {
    context    = each.value.context
    dockerfile = each.value.dockerfile
    tag        = [each.value.tag]
    build_args = each.value.build_args
    platform   = each.value.platform
  }

  # The provider hashes the entire build context to decide "did
  # anything change", which covers Dockerfile edits since the
  # Dockerfile lives inside the context. `triggers` is an additional
  # rebuild signal — kept here so editing a build_arg or platform
  # via var.images forces a rebuild even when the on-disk context
  # tree is byte-identical.
  triggers = {
    spec_hash = sha256(jsonencode({
      tag        = each.value.tag
      dockerfile = each.value.dockerfile
      build_args = each.value.build_args
      platform   = each.value.platform
    }))
  }
}
