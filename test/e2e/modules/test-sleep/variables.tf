variable "enabled" {
  type        = bool
  description = "When false, the module is a no-op — no file is created, nothing blocks. Default off so CI doesn't hang; flip to true for local dev pause-for-inspection."
  default     = false
}

variable "file_sleep_path" {
  type        = string
  description = "Path for the sleep file. If null, defaults to $${path.root}/.tofu-sleep (i.e. test/e2e/.tofu-sleep under `tofu test`). Delete the file to unblock the apply. Ignored when enabled=false."
  default     = null
}
