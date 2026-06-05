locals {
  # Default lands in the caller's root module dir (test/e2e/ when invoked
  # from `tofu test`) so the sleep file is visible in the repo tree and
  # easy to `rm` without remembering a /tmp/ path. Override via
  # var.file_sleep_path if you need a different location (e.g. when
  # running two concurrent sleeps that would otherwise collide).
  file_sleep_path = var.enabled ? coalesce(var.file_sleep_path, "${path.root}/.tofu-sleep") : null
}

resource "terraform_data" "file_sleep" {
  count = var.enabled ? 1 : 0
  input = local.file_sleep_path

  provisioner "local-exec" {
    command = <<-EOT
      touch "$SLEEP_FILE"
      printf 'File created at: %s\nDelete this file to continue...\n' "$SLEEP_FILE"
      trap 'echo "Interrupted. Continuing teardown..."; rm -f "$SLEEP_FILE"; exit 0' INT TERM
      while [ -f "$SLEEP_FILE" ]; do sleep 2; done
      echo 'File deleted. Continuing...'
    EOT
    environment = {
      SLEEP_FILE = self.input
    }
  }
}
