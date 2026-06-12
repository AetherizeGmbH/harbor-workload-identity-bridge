locals {
  # A fresh random suffix on every plan. uuid() is re-evaluated each plan
  # and never matches prior state, so the sleep file path changes every
  # run and terraform_data below is forced to recreate (re-running the
  # wait provisioner). Hex slice keeps the filename short and tidy.
  sleep_suffix = substr(sha256(uuid()), 0, 8)

  # Default lands in the directory the user typed `tofu test` from
  # (path.cwd) — under our harness that's test/e2e/. path.root would
  # have been more semantically correct, but in tftest with a module{}
  # override path.root resolves to the OVERRIDE module's directory, so
  # the file would land inside this module's own dir — not user-
  # friendly. path.cwd is stable across run blocks. Override the base via
  # var.file_sleep_path if you need a different location; the random
  # suffix is appended regardless.
  file_sleep_path = var.enabled ? "${coalesce(var.file_sleep_path, "${path.cwd}/.tofu-sleep")}-${local.sleep_suffix}" : null
}

resource "terraform_data" "file_sleep" {
  count = var.enabled ? 1 : 0
  input = local.file_sleep_path

  # Recreate whenever the suffix changes — i.e. every plan — so the
  # wait provisioner re-runs and blocks again on a fresh file.
  triggers_replace = local.sleep_suffix

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
