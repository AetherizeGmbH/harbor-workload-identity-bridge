variable "enabled" {
  type        = bool
  description = "When false, the module is a no-op — no file is created, nothing blocks. Default off so CI doesn't hang; flip to true for local dev pause-for-inspection."
  default     = false
}

variable "file_sleep_path" {
  type        = string
  description = "Base path for the sleep file. If null, defaults to $${path.cwd}/.tofu-sleep — i.e. test/e2e/.tofu-sleep when invoked via `cd test/e2e && tofu test`. A random suffix is appended on every plan, so the real file is e.g. .tofu-sleep-1a2b3c4d (see the file_sleep_path output for the exact path). Delete that file to unblock the apply. Ignored when enabled=false."
  default     = null
}
