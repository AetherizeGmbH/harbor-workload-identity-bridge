# Root module for the OpenTofu e2e harness. The actual orchestration
# lives in tests/01-bridge.tftest.hcl, which invokes each child module
# via `run "..." { module { source = "./modules/..." } }`. Each `run`
# block swaps the active module — `var.X` inside refers to that
# child's variables, not the test root's — so values are passed in
# directly as literals or `run.<previous>.<output>` references.
terraform {
  required_version = ">= 1.12"
}

# File-scope variables for the tftest files. Declaring them here makes
# them settable via `tofu test -var ...` or TF_VAR_* env vars. CI gets
# the defaults; local dev flips them on as needed.

variable "pause_after_pull" {
  type        = bool
  description = "When true, the test pauses AFTER all the pull/push assertions via the test-sleep module — a file appears at test/e2e/.tofu-sleep and the apply blocks until you `rm` it. Lets you kubectl-poke a fully-populated cluster (robots, Secrets, pushed tags). Off in CI; flip on via TF_VAR_pause_after_pull=true."
  default     = false
}
