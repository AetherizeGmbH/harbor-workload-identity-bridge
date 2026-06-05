variable "images" {
  type = map(object({
    tag        = string
    dockerfile = string
    context    = string
    build_args = optional(map(string), {})
    platform   = optional(string, null) # e.g. "linux/amd64" — omit for the host's native platform
  }))
  description = <<-EOT
    Images to build with `docker build`. Map keys are friendly names referenced in outputs.

      tag        — image:tag to produce locally
      dockerfile — path to the Dockerfile (any path docker accepts)
      context    — build context directory
      build_args — optional --build-arg key/value map
      platform   — optional --platform value (e.g. linux/amd64); omit for host-native
  EOT
}
