locals {
  # Default lands in the directory the user typed `tofu test` from
  # (path.cwd) — under our harness that's test/e2e/. path.root would
  # have been more semantically correct, but in tftest with a module{}
  # override path.root resolves to the OVERRIDE module's directory, so
  # the file would land inside this module's own dir — not user-
  # friendly. path.cwd is stable across run blocks. Override via
  # var.file_sleep_path if you need a different location.
  file_sleep_path = var.enabled ? coalesce(var.file_sleep_path, "${path.cwd}/.tofu-sleep") : null
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
